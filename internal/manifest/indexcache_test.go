package manifest

import (
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
)

type costCache struct{ costs map[interface{}]int64 }

func (c *costCache) SetWithTTL(k, _ interface{}, cost int64, _ time.Duration) bool {
	c.costs[k] = cost
	return true
}
func (c *costCache) Get(interface{}) (interface{}, bool) { return nil, false }

func TestIndexCost(t *testing.T) {
	const budget = 128 << 20
	if got := IndexCost(1000, nil, budget); got != 2000 {
		t.Errorf("parsed index: %d, want 2000", got)
	}
	if got := IndexCost(200<<20, nil, budget); got != budget {
		t.Errorf("oversized index: %d, want capped at %d", got, budget)
	}
	if got := IndexCost(1<<20, errors.New("boom"), budget); got > 1024 {
		t.Errorf("error entry: %d, want a small fixed cost", got)
	}
}

// A large index that fails to parse used to be charged twice its size, which
// could exceed the budget and keep the error from being cached at all.
func TestGetIndex_ParseFailureIsChargedAsError(t *testing.T) {
	// UnmarshalStrict rejects the unknown field; the comment makes it large.
	body := "# " + strings.Repeat("x", 1<<20) + "\nentries: {}\nnotAnIndexField: 1\n"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	cc := &costCache{costs: map[interface{}]int64{}}
	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{IndexCacheMaxCost: 128 << 20, IndexErrorCacheTTL: time.Minute}, cc, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = srv.Client()
	host := strings.TrimPrefix(srv.URL, "https://")
	if _, err := m.GetIndex(host); err == nil {
		t.Fatal("expected a parse error")
	}
	if got := cc.costs[host]; got <= 0 || got > 1024 {
		t.Errorf("cost of failed parse = %d, want a small error cost", got)
	}
}

// ParseIndex returns the parsed entries together with ErrNoAPIVersion; the
// cached error entry must not keep them.
func TestGetIndex_PartialIndexIsNotCached(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("apiVersion: \"\"\nentries:\n  demo:\n    - name: demo\n      version: 1.0.0\n      urls: [demo-1.0.0.tgz]\n"))
	}))
	t.Cleanup(srv.Close)
	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{IndexCacheMaxCost: 128 << 20}, &costCache{costs: map[interface{}]int64{}}, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = srv.Client()
	idx, err := m.GetIndex(strings.TrimPrefix(srv.URL, "https://"))
	if err == nil {
		t.Fatal("expected ErrNoAPIVersion")
	}
	if idx != nil {
		t.Errorf("partial index with %d charts returned and cached alongside the error", len(idx.Entries))
	}
}
