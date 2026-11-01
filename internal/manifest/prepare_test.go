package manifest

import (
	"bytes"
	"fmt"
	"io"
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
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

type syncMapCache struct {
	mu sync.Mutex
	m  map[interface{}]interface{}
}

func (c *syncMapCache) SetWithTTL(k, v interface{}, _ int64, _ time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
	return true
}

func (c *syncMapCache) Get(k interface{}) (interface{}, bool) {
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
	m, repo := twoVersionServer(t, &syncMapCache{m: map[interface{}]interface{}{}}, func(w http.ResponseWriter, _ *http.Request) {
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

// Only a definitive 404 or 403 is remembered; an upstream that failed once
// must be asked again on the next request.
func TestPrepare_TransientFailureIsRetried(t *testing.T) {
	tgz := chartTarball(t, "demo", "1.0.0")
	var downloads atomic.Int32
	m, repo := twoVersionServer(t, &syncMapCache{m: map[interface{}]interface{}{}}, func(w http.ResponseWriter, _ *http.Request) {
		if downloads.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(tgz)
	})
	if err := getVersion(t, m, repo, "1.0.0"); err == nil {
		t.Fatal("expected the first request to fail")
	}
	if err := getVersion(t, m, repo, "1.0.0"); err != nil {
		t.Fatalf("second request: %v (a transient failure was cached)", err)
	}
}

// Each preparation holds an upstream connection, so a sweep over many versions must not
// start them all at once.
func TestPrepare_ConcurrencyIsBounded(t *testing.T) {
	tgz := chartTarball(t, "demo", "1.0.0")
	var inflight, peak atomic.Int32
	m, repo := twoVersionServer(t, nopCache{}, func(w http.ResponseWriter, _ *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write(tgz)
	})
	m.slots = make(chan struct{}, 1)
	var wg sync.WaitGroup
	for _, v := range []string{"1.0.0", "2.0.0"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := getVersion(t, m, repo, v); err != nil {
				t.Errorf("GET %s: %v", v, err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Errorf("%d tarball downloads ran at once, want 1", p)
	}
}

// A version constraint resolves to a concrete version that is stored under
// that version, so the requested name never appears; the 404 must be cached
// instead of downloading the chart on every request.
func TestPrepare_ConstraintTargetIsNotRedownloaded(t *testing.T) {
	tgz := chartTarball(t, "demo", "1.0.0")
	var downloads atomic.Int32
	m, repo := twoVersionServer(t, nopCache{}, func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(tgz)
	})
	for range 3 {
		_ = getVersion(t, m, repo, "1.0")
	}
	if n := downloads.Load(); n != 1 {
		t.Errorf("%d tarball downloads for 3 requests of a constraint, want 1", n)
	}
}

// panickingCache panics on the flight goroutine, where index lookups run.
type panickingCache struct{}

func (panickingCache) SetWithTTL(_, _ interface{}, _ int64, _ time.Duration) bool { return false }
func (panickingCache) Get(_ interface{}) (interface{}, bool)                      { panic("cache failure") }

// singleflight re-panics on a goroutine of its own, which would take down the
// whole process instead of failing one request.
func TestPrepare_PanicFailsTheRequestOnly(t *testing.T) {
	m, repo := twoVersionServer(t, panickingCache{}, func(w http.ResponseWriter, _ *http.Request) {})
	if err := getVersion(t, m, repo, "1.0.0"); err == nil {
		t.Fatal("expected an error from a panicking preparation")
	}
}

// The digest and tag entries of a chart reference the same blobs; expiring one
// of them must not delete blobs the other still serves.
func TestCleanup_KeepsBlobsOfLiveManifests(t *testing.T) {
	blobs := mem.NewMemHandler()
	m := NewManifests(t.Context(), blobs, Config{CacheTTL: time.Minute}, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))
	data := []byte("chart layer")
	h, _, err := v1.SHA256(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(t.Context(), "", h, io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	_ = m.Write("r/demo", "sha256:abc", Manifest{Refs: []string{h.String()}, CreatedAt: old})
	_ = m.Write("r/demo", "1.0.0", Manifest{Refs: []string{h.String()}, CreatedAt: time.Now()})

	m.cleanup(t.Context())
	if _, err := blobs.Stat(t.Context(), "", h); err != nil {
		t.Fatalf("blob of a live manifest was deleted: %v", err)
	}

	_ = m.Write("r/demo", "1.0.0", Manifest{Refs: []string{h.String()}, CreatedAt: old})
	m.cleanup(t.Context())
	if _, err := blobs.Stat(t.Context(), "", h); err == nil {
		t.Error("blob still stored after every manifest referencing it expired")
	}
	if names := m.names("r/demo"); len(names) != 0 {
		t.Errorf("expired manifests still listed: %v", names)
	}
}
