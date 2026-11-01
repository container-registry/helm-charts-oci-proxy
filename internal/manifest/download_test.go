package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
)

// chartServer serves an index with one version of chart "demo" whose tarball
// is answered by the given handler. digest is written into the index as-is.
func chartServer(t *testing.T, digest string, tgz http.HandlerFunc) (*Manifests, string) {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `apiVersion: v1
entries:
  demo:
    - apiVersion: v2
      name: demo
      version: 1.0.0
      digest: %q
      urls:
        - %s/demo-1.0.0.tgz
`, digest, srv.URL)
	})
	mux.HandleFunc("/demo-1.0.0.tgz", tgz)
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{}, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = srv.Client()
	return m, strings.TrimPrefix(srv.URL, "https://") + "/demo"
}

func getManifest(t *testing.T, m *Manifests, repo string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+repo+"/manifests/1.0.0", nil)
	return rec, m.Handle(rec, req)
}

func regStatus(t *testing.T, err error) int {
	t.Helper()
	var re *errors.RegError
	if !stderrors.As(err, &re) {
		t.Fatalf("expected *errors.RegError, got %T: %v", err, err)
	}
	return re.Status
}

// Before this change an upstream error page was read as the chart, packaged
// and served with 200; clients cached an artifact that cannot be installed.
func TestPrepareChart_UpstreamErrorIsNeverPackaged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream http.HandlerFunc
		want     int
	}{
		{"404 page", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<!DOCTYPE html><html>not found</html>")
		}, http.StatusNotFound},
		{"410", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }, http.StatusNotFound},
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, http.StatusBadGateway},
		{"200 with HTML", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "<!DOCTYPE html><html>soft 404</html>")
		}, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, repo := chartServer(t, "", tc.upstream)
			rec, err := getManifest(t, m, repo)
			if err == nil {
				t.Fatalf("expected an error, got status %d", rec.Code)
			}
			if got := regStatus(t, err); got != tc.want {
				t.Errorf("status = %d, want %d (%v)", got, tc.want, err)
			}
			if n := len(m.manifests[repo]); n != 0 {
				t.Errorf("%d manifests stored for a failed download", n)
			}
		})
	}
}

// Helm does not verify the index digest on pull, and public indexes carry
// stale digests for working tarballs, so a mismatch must not fail the pull.
func TestPrepareChart_IndexDigestMismatchIsObservedOnly(t *testing.T) {
	tgz := chartTarball(t, "demo", "1.0.0")
	sum := sha256.Sum256(tgz)
	serve := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }

	for _, tc := range []struct {
		name     string
		digest   string
		mismatch int64
	}{
		{"matching", hex.EncodeToString(sum[:]), 0},
		{"matching with prefix", "sha256:" + hex.EncodeToString(sum[:]), 0},
		{"stale", strings.Repeat("0", 64), 1},
		{"absent", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, repo := chartServer(t, tc.digest, serve)
			before := chartDigestMismatches.Value()
			rec, err := getManifest(t, m, repo)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := chartDigestMismatches.Value() - before; got != tc.mismatch {
				t.Errorf("chart_digest_mismatches grew by %d, want %d", got, tc.mismatch)
			}
		})
	}
}
