package manifest

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"helm.sh/helm/v3/pkg/chart"
)

// rewriteUpstream serves one chart version that has a rewritable dependency.
func rewriteUpstream(t *testing.T) (repo string, client *http.Client) {
	t.Helper()
	tgz, err := createTestTarball("demo", &chart.Metadata{
		APIVersion: "v2", Name: "demo", Version: "1.0.0",
		Dependencies: []*chart.Dependency{{Name: "redis", Version: "17.0.0", Repository: "https://charts.example.com/stable"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - apiVersion: v2\n      name: demo\n      version: 1.0.0\n      urls: [%s/demo-1.0.0.tgz]\n", srv.URL)
	})
	mux.HandleFunc("/demo-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) })
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://") + "/demo", srv.Client()
}

func rewriteManifests(t *testing.T, cfg Config, client *http.Client) *Manifests {
	t.Helper()
	m := NewManifests(t.Context(), mem.NewMemHandler(), cfg, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = client
	m.layerClient = newLayerClient(client)
	return m
}

func manifestDigest(t *testing.T, m *Manifests, repo, host, query string) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+repo+"/manifests/1.0.0"+query, nil)
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	if err := m.Handle(rec, req); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return rec.Header().Get("Docker-Content-Digest")
}

var rewriteConfig = Config{RewriteDependencies: true, ProxyHost: "proxy.example.com"}

// A prepared chart is cached for every client, so a single request must not
// be able to decide whether or how its dependencies are rewritten.
func TestRequestCannotEnableDependencyRewriting(t *testing.T) {
	repo, client := rewriteUpstream(t)
	plain := manifestDigest(t, rewriteManifests(t, Config{}, client), repo, "", "")
	if rewritten := manifestDigest(t, rewriteManifests(t, rewriteConfig, client), repo, "", ""); rewritten == plain {
		t.Fatal("rewriting did not change the manifest; the test chart proves nothing")
	}

	m := rewriteManifests(t, Config{}, client)
	if got := manifestDigest(t, m, repo, "evil.example", "?rewrite_dependencies=true"); got != plain {
		t.Errorf("request with rewrite_dependencies=true got %s, want %s", got, plain)
	}
	if got := manifestDigest(t, m, repo, "", ""); got != plain {
		t.Errorf("later plain request got %s, want %s", got, plain)
	}
}

func TestRewrittenDependenciesIgnoreRequestHost(t *testing.T) {
	repo, client := rewriteUpstream(t)
	want := manifestDigest(t, rewriteManifests(t, rewriteConfig, client), repo, "", "")

	m := rewriteManifests(t, rewriteConfig, client)
	if got := manifestDigest(t, m, repo, "evil.example", "?rewrite_dependencies=false"); got != want {
		t.Errorf("request with Host evil.example got %s, want %s", got, want)
	}
	if got := manifestDigest(t, m, repo, "", ""); got != want {
		t.Errorf("later plain request got %s, want %s", got, want)
	}
}
