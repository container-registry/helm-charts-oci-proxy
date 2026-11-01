package manifest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	var gzHTML bytes.Buffer
	zw := gzip.NewWriter(&gzHTML)
	if _, err := zw.Write([]byte("<!DOCTYPE html><html>not a chart</html>")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	tgz := chartTarball(t, "demo", "1.0.0")
	// Keeps the gzip header, so only full archive parsing can reject it.
	truncated := tgz[:len(tgz)/2]

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
		{"200 with gzip-compressed HTML", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(gzHTML.Bytes())
		}, http.StatusBadGateway},
		{"200 with truncated archive", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(truncated)
		}, http.StatusBadGateway},
		// The declared length is never sent, so the client fails reading the body.
		{"body cut short", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100000")
			_, _ = w.Write(truncated)
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

// An index entry that points to an OCI registry cannot be converted. Before
// this change the proxy refused the oci:// URL as a non-https download and
// answered 500, which clients and Harbor treat as a server fault and retry.
func TestPrepareChart_OCIChartURLIsNotFound(t *testing.T) {
	var downloads int
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `apiVersion: v1
entries:
  demo:
    - apiVersion: v2
      name: demo
      version: 1.0.0
      urls:
        - oci://registry.example.com/charts/demo:1.0.0
`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{}, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = srv.Client()
	repo := strings.TrimPrefix(srv.URL, "https://") + "/demo"

	rec, err := getManifest(t, m, repo)
	if err == nil {
		t.Fatalf("expected an error, got status %d", rec.Code)
	}
	if got := regStatus(t, err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "oci://registry.example.com/charts/demo:1.0.0") {
		t.Errorf("error does not name the OCI location: %v", err)
	}
	if downloads != 0 {
		t.Errorf("%d upstream requests besides the index", downloads)
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
		{"uppercase prefix", "SHA256:" + hex.EncodeToString(sum[:]), 0},
		{"other algorithm", "sha512:" + strings.Repeat("0", 128), 0},
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

func TestDownloadError_Mapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"upstream 404", &upstreamStatusError{status: http.StatusNotFound}, http.StatusNotFound},
		{"upstream 503", &upstreamStatusError{status: http.StatusServiceUnavailable}, http.StatusBadGateway},
		{"unknown host", &url.Error{Op: "Get", Err: &net.DNSError{IsNotFound: true}}, http.StatusNotFound},
		{"private dial refused", &url.Error{Op: "Get", Err: &net.OpError{Op: "dial", Err: &policyError{"refusing"}}}, http.StatusForbidden},
		{"timeout", &url.Error{Op: "Get", Err: stderrors.New("timeout")}, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := downloadError(tc.err, "NAME_UNKNOWN").Status; got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// Before, an index fetch failure of any kind answered 404, so an upstream
// outage looked like a repository that does not exist.
func TestPrepareChart_IndexOutageIsBadGateway(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	m := NewManifests(t.Context(), mem.NewMemHandler(), Config{}, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))
	m.httpClient = srv.Client()
	_, err := getManifest(t, m, strings.TrimPrefix(srv.URL, "https://")+"/demo")
	if got := regStatus(t, err); got != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (%v)", got, err)
	}
}

func TestDownload_BodyIsBounded(t *testing.T) {
	old := maxDownloadSize
	maxDownloadSize = 1 << 10
	t.Cleanup(func() { maxDownloadSize = old })
	m, repo := chartServer(t, "", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{0x1f}, 2<<10))
	})
	_, err := getManifest(t, m, repo)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}
