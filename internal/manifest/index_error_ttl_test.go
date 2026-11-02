package manifest

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"github.com/dgraph-io/ristretto"
)

// INDEX_ERROR_CACHE_TTL=0 means a failed index is not cached; ristretto would
// keep an entry with TTL 0 forever.
func TestZeroIndexErrorCacheTTLDoesNotCacheErrors(t *testing.T) {
	var downloads atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	cache, err := ristretto.NewCache(&ristretto.Config{NumCounters: 1e3, MaxCost: 1 << 20, BufferItems: 64, IgnoreInternalCost: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{IndexCacheMaxCost: 1 << 20}, cache, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = srv.Client()
	repo := strings.TrimPrefix(srv.URL, "https://") + "/charts"
	for range 2 {
		if _, err := m.GetIndex(repo); err == nil {
			t.Fatal("expected an index error")
		}
	}
	if n := downloads.Load(); n != 2 {
		t.Errorf("%d index downloads, want 2: the error was cached", n)
	}
}

func TestZeroIndexErrorCacheTTLDoesNotCacheVersionFailures(t *testing.T) {
	c := newFailureCache(0)
	c.set("repo\x00ref", resultNotFound("repo", "ref"))
	if err := c.get("repo\x00ref"); err != nil {
		t.Fatalf("failure cached with TTL 0: %v", err)
	}
}
