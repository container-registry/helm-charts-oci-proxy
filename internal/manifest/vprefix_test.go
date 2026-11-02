package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// versionsServer serves an index of chart demo with the given versions, each
// with a tarball of its own whose Chart.yaml carries that exact version.
func versionsServer(t *testing.T, versions ...string) (*Manifests, string, map[string]string, *atomic.Int32) {
	t.Helper()
	var srv *httptest.Server
	var downloads atomic.Int32
	digests := map[string]string{}
	mux := http.NewServeMux()
	var entries strings.Builder
	for _, v := range versions {
		if _, dup := digests[v]; dup {
			fmt.Fprintf(&entries, "    - apiVersion: v2\n      name: demo\n      version: %s\n      urls: [{{.}}/demo-%s.tgz]\n", v, v)
			continue
		}
		tgz := chartTarball(t, "demo", v)
		sum := sha256.Sum256(tgz)
		digests[v] = "sha256:" + hex.EncodeToString(sum[:])
		mux.HandleFunc("/demo-"+v+".tgz", func(w http.ResponseWriter, _ *http.Request) {
			downloads.Add(1)
			_, _ = w.Write(tgz)
		})
		fmt.Fprintf(&entries, "    - apiVersion: v2\n      name: demo\n      version: %s\n      urls: [{{.}}/demo-%s.tgz]\n", v, v)
	}
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "apiVersion: v1\nentries:\n  demo:\n"+strings.ReplaceAll(entries.String(), "{{.}}", srv.URL))
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{}, &syncMapCache{m: map[interface{}]interface{}{}}, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = srv.Client()
	return m, strings.TrimPrefix(srv.URL, "https://") + "/demo", digests, &downloads
}

func layerDigest(t *testing.T, m *Manifests, repo, tag string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+repo+"/manifests/"+tag, nil)
	if err := m.Handle(rec, req); err != nil {
		t.Fatalf("GET manifest %s: %v", tag, err)
	}
	var ma ocispec.Manifest
	if err := json.Unmarshal(rec.Body.Bytes(), &ma); err != nil || len(ma.Layers) != 1 {
		t.Fatalf("manifest %s: %v %s", tag, err, rec.Body.Bytes())
	}
	return ma.Layers[0].Digest.String()
}

func listedTags(t *testing.T, m *Manifests, repo string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+repo+"/tags/list", nil)
	if err := m.HandleTags(rec, req); err != nil {
		t.Fatalf("tags/list: %v", err)
	}
	var got listTags
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("tags/list: %v", err)
	}
	return got.Tags
}

// charts.rook.io lists 1.21.1 and v1.21.1 as different tarballs. Each tag must
// reach its own chart whichever is requested first, and tags/list must offer
// both once, also when the index repeats an entry.
func TestHandle_VPrefixedAndPlainVersionsAreDistinct(t *testing.T) {
	for _, order := range [][]string{
		{"v1.21.1", "1.21.1", "v1.21.1", "1.21.1"},
		{"1.21.1", "v1.21.1", "1.21.1", "v1.21.1"},
	} {
		t.Run(order[0]+"-first", func(t *testing.T) {
			m, repo, want, _ := versionsServer(t, "1.21.1", "v1.21.1", "1.21.1")
			for _, tag := range order {
				if got := layerDigest(t, m, repo, tag); got != want[tag] {
					t.Errorf("tag %s: layer %s, want %s", tag, got, want[tag])
				}
			}
			if got := listedTags(t, m, repo); !slices.Equal(got, []string{"1.21.1", "v1.21.1"}) {
				t.Errorf("tags/list = %v, want [1.21.1 v1.21.1]", got)
			}
		})
	}
}

// Without a collision a tag still reaches the version with the leading "v"
// added or removed, both spellings share one download, and tags/list keeps
// the strict semver form helm 3 resolves "latest" and constraints from.
// Exact-first reuses the cached manifest for the alias; alias-first stores the
// alias while preparing the chart.
func TestHandle_VPrefixFallbackSharesOneDownload(t *testing.T) {
	type req struct{ tag, version string }
	for name, order := range map[string][]req{
		"exact-first": {{"v2.0.0", "v2.0.0"}, {"2.0.0", "v2.0.0"}, {"1.0.0", "1.0.0"}, {"v1.0.0", "1.0.0"}},
		"alias-first": {{"2.0.0", "v2.0.0"}, {"v2.0.0", "v2.0.0"}, {"v1.0.0", "1.0.0"}, {"1.0.0", "1.0.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			m, repo, want, downloads := versionsServer(t, "v2.0.0", "1.0.0")
			for _, tc := range order {
				if got := layerDigest(t, m, repo, tc.tag); got != want[tc.version] {
					t.Errorf("tag %s: layer %s, want %s", tc.tag, got, want[tc.version])
				}
			}
			if n := downloads.Load(); n != 2 {
				t.Errorf("%d tarball downloads for 2 versions, want 2", n)
			}
			if got := listedTags(t, m, repo); !slices.Equal(got, []string{"1.0.0", "2.0.0"}) {
				t.Errorf("tags/list = %v, want [1.0.0 2.0.0]", got)
			}
		})
	}
}
