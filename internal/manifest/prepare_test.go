package manifest

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
)

type mapCache struct {
	mu sync.Mutex
	m  map[interface{}]interface{}
}

func (c *mapCache) SetWithTTL(k, v interface{}, _ int64, _ time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
	return true
}

func (c *mapCache) Get(k interface{}) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

// twoVersionServer serves an index with demo 1.0.0 and 2.0.0; tgz handles
// every tarball request.
func twoVersionServer(t *testing.T, cache Cache, tgz http.HandlerFunc) (*Manifests, string) {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `apiVersion: v1
entries:
  demo:
    - apiVersion: v2
      name: demo
      version: 2.0.0
      urls: [%[1]s/demo-2.0.0.tgz]
    - apiVersion: v2
      name: demo
      version: 1.0.0
      urls: [%[1]s/demo-1.0.0.tgz]
`, srv.URL)
	})
	mux.HandleFunc("/demo-1.0.0.tgz", tgz)
	mux.HandleFunc("/demo-2.0.0.tgz", tgz)
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{IndexErrorCacheTTl: time.Minute}, cache, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = srv.Client()
	return m, strings.TrimPrefix(srv.URL, "https://") + "/demo"
}

func getVersion(t *testing.T, m *Manifests, repo, version string) error {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+repo+"/manifests/"+version, nil)
	return m.Handle(rec, req)
}

// A sweep sends many requests for the same version at once; they must share
// one upstream download instead of each preparing the chart.
func TestPrepare_ConcurrentSameVersionDownloadsOnce(t *testing.T) {
	tgz := chartTarball(t, "demo", "1.0.0")
	var downloads atomic.Int32
	m, repo := twoVersionServer(t, nopCache{}, func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write(tgz)
	})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := getVersion(t, m, repo, "1.0.0"); err != nil {
				t.Errorf("GET: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := downloads.Load(); n != 1 {
		t.Errorf("%d tarball downloads for 20 concurrent requests, want 1", n)
	}
}

// Before, one global lock serialised every manifest request, so a slow
// upstream for one version stalled all others.
func TestPrepare_DifferentVersionsRunInParallel(t *testing.T) {
	tgz := chartTarball(t, "demo", "1.0.0")
	var arrived atomic.Int32
	both := make(chan struct{})
	m, repo := twoVersionServer(t, nopCache{}, func(w http.ResponseWriter, _ *http.Request) {
		if arrived.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
			_, _ = w.Write(tgz)
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	var wg sync.WaitGroup
	for _, v := range []string{"1.0.0", "2.0.0"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := getVersion(t, m, repo, v); err != nil {
				t.Errorf("GET %s: %v (versions were not prepared in parallel)", v, err)
			}
		}()
	}
	wg.Wait()
}

// A missing version is answered from the error cache instead of reaching the
// upstream on every request of a sweep.
func TestPrepare_FailureIsCached(t *testing.T) {
	var downloads atomic.Int32
	m, repo := twoVersionServer(t, &mapCache{m: map[interface{}]interface{}{}}, func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	for range 3 {
		if err := getVersion(t, m, repo, "1.0.0"); err == nil {
			t.Fatal("expected 404")
		}
	}
	if n := downloads.Load(); n != 1 {
		t.Errorf("%d tarball downloads for 3 requests of a missing version, want 1", n)
	}
}
