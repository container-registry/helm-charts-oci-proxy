package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
)

// chartTarball returns a minimal gzipped chart archive containing only Chart.yaml.
func chartTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	meta := fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", name, version)
	if err := tw.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0o644, Size: int64(len(meta))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(meta)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestHandle_BuildMetadataVersion guards the "+" handling. Helm requests a
// chart version such as 1.0.0+build7 as the OCI tag 1.0.0_build7, and expects
// tags/list to use the same form. Before this test the GET branch only knew the
// "+" form, so `helm pull` of any version with build metadata failed with 404
// while HEAD on the same tag succeeded.
func TestHandle_BuildMetadataVersion(t *testing.T) {
	const version = "1.0.0+build7"
	tgz := chartTarball(t, "plusdemo", version)

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `apiVersion: v1
entries:
  plusdemo:
    - apiVersion: v2
      name: plusdemo
      version: %s
      urls:
        - %s/plusdemo-%s.tgz
`, version, srv.URL, version)
	})
	mux.HandleFunc("/plusdemo-"+version+".tgz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tgz)
	})
	srv = httptest.NewTLSServer(mux)
	defer srv.Close()

	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{}, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))
	// The test server is on loopback with a self-signed certificate, which the
	// production download client refuses by design.
	m.httpClient = srv.Client()

	repo := strings.TrimPrefix(srv.URL, "https://") + "/plusdemo"
	manifestURL := func(tag string) string { return "/v2/" + repo + "/manifests/" + tag }

	digests := map[string]string{}
	for _, tc := range []struct {
		method, tag string
	}{
		{http.MethodHead, "1.0.0_build7"},
		{http.MethodGet, "1.0.0_build7"},
		{http.MethodGet, "1.0.0+build7"},
		{http.MethodGet, "v1.0.0_build7"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), tc.method, manifestURL(tc.tag), nil)
		if err := m.Handle(rec, req); err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.tag, err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d", tc.method, tc.tag, rec.Code)
		}
		digests[tc.method+" "+tc.tag] = rec.Header().Get("Docker-Content-Digest")
	}
	for k, d := range digests {
		if d == "" || d != digests["GET 1.0.0+build7"] {
			t.Errorf("%s: digest %q differs from the canonical manifest digest %q", k, d, digests["GET 1.0.0+build7"])
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+repo+"/tags/list", nil)
	if err := m.HandleTags(rec, req); err != nil {
		t.Fatalf("tags/list: %v", err)
	}
	var got listTags
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding tags/list: %v", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "1.0.0_build7" {
		t.Errorf("tags/list = %v, want [1.0.0_build7]", got.Tags)
	}
}
