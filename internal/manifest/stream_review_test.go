package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/container-registry/helm-charts-oci-proxy/internal/registry"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart/loader"
)

func tarballOf(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (f *streamFixture) layer(t *testing.T) digest.Digest {
	t.Helper()
	_, body := f.manifest(t, "")
	var man ocispec.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		t.Fatal(err)
	}
	return man.Layers[0].Digest
}

// helm rejects a chart whose values.yaml does not parse; the streamed path
// must not build a manifest for it either.
func TestReadChartMetadata_RejectsWhatHelmRejects(t *testing.T) {
	tgz := tarballOf(t, [][2]string{
		{"demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: 1.0.0\n"},
		{"demo/values.yaml", "a: [\n"},
	})
	if _, err := loader.LoadArchive(bytes.NewReader(tgz)); err == nil {
		t.Fatal("helm accepted the broken chart; the test chart is wrong")
	}
	if _, err := readChartMetadata(bytes.NewReader(tgz)); err == nil {
		t.Fatal("readChartMetadata accepted a chart helm rejects")
	}
}

// The size bound of the streamed path, which no longer goes through download.
func TestPrepareStreamed_BodyIsBounded(t *testing.T) {
	// Random content does not compress, so the archive is larger than the
	// bound while index.yaml stays well below it.
	noise := make([]byte, 4<<10)
	_, _ = rand.Read(noise)
	old := maxDownloadSize
	maxDownloadSize = 2 << 10
	t.Cleanup(func() { maxDownloadSize = old })
	f := newStreamFixture(t)
	f.serve.Store(tarballOf(t, [][2]string{
		{"demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: 1.0.0\n"},
		{"demo/templates/noise.txt", string(noise)},
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+f.repo+"/manifests/1.0.0", nil)
	err := f.m.Handle(rec, req)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

// A layer is streamed at the client's pace, so the download client's overall
// timeout must not cut it off.
func TestStreamedLayer_OutlivesDownloadTimeout(t *testing.T) {
	f := newStreamFixture(t)
	d := f.layer(t)
	f.m.httpClient = &http.Client{Transport: f.m.httpClient.Transport, Timeout: 100 * time.Millisecond}
	f.m.layerClient = newLayerClient(f.m.httpClient)
	f.delay.Store(int64(300 * time.Millisecond))
	rc, err := f.open(t, d)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("slow layer read failed: %v", err)
	}
}

// A republished upstream file of the same size must not reach the client as a
// complete 200 response.
func TestStreamedLayer_BadBodyIsAbortedOverHTTP(t *testing.T) {
	f := newStreamFixture(t)
	d := f.layer(t)
	changed := bytes.Clone(f.tgz)
	changed[len(changed)-1] ^= 0xff
	f.serve.Store(changed)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = f.m.HandleBlobs(w, r)
	}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/v2/" + f.repo + "/blobs/" + d.String())
	if err != nil {
		// Small bodies sit in net/http's write buffer, so the abort drops the
		// connection before even the status line is sent.
		return
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("client received a complete %d response of %d bytes with the wrong content", resp.StatusCode, len(got))
	}
}

// An upstream outage is an upstream fault, as on the manifest path.
func TestStreamedLayer_UpstreamFailureIsBadGateway(t *testing.T) {
	f := newStreamFixture(t)
	d := f.layer(t)
	for _, tc := range []struct{ upstream, want int }{
		{http.StatusServiceUnavailable, http.StatusBadGateway},
		{http.StatusNotFound, http.StatusNotFound},
	} {
		f.status.Store(int32(tc.upstream))
		_, err := f.open(t, d)
		var re *errors.RegError
		if !stderrors.As(err, &re) || re.Status != tc.want {
			t.Errorf("upstream %d: got %v, want a %d RegError", tc.upstream, err, tc.want)
		}
	}
}

const testRepo = "charts.example.com/demo"

// layerUpstream serves bodies by path over TLS and answers 404 for any other
// path. requests counts every request.
func layerUpstream(t *testing.T, bodies map[string][]byte) (m *Manifests, base string, requests *atomic.Int32) {
	t.Helper()
	requests = &atomic.Int32{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if b, ok := bodies[r.URL.Path]; ok {
			_, _ = w.Write(b)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	m = newTestManifests(t, Config{})
	m.layerClient = newLayerClient(srv.Client())
	return m, srv.URL, requests
}

// holdLayer stores a chart version of testRepo whose layer is content, kept in
// memory when data is set and streamed from urls otherwise.
func holdLayer(m *Manifests, version string, content []byte, data bool, urls ...string) digest.Digest {
	l := &blob{digest: digest.FromBytes(content), size: int64(len(content)), urls: urls}
	if data {
		l.data = content
	}
	m.insert(testRepo, &record{layer: l, expires: time.Now().Add(time.Hour)}, version)
	return l.digest
}

func readBlob(t *testing.T, m *Manifests, d digest.Digest) ([]byte, error) {
	t.Helper()
	b, ok := m.findBlob(testRepo, d)
	if !ok {
		t.Fatalf("blob %s is not held", d)
	}
	rc, err := m.openBlob(t.Context(), testRepo, b)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Two versions of a repository publishing the same tarball share its digest;
// losing one upstream file must not break the other's layer.
func TestStreamBlobs_FallsBackToOtherSources(t *testing.T) {
	data := []byte("shared tarball")
	m, base, _ := layerUpstream(t, map[string][]byte{"/b.tgz": data})
	holdLayer(m, "1.0.0", data, false, base+"/a.tgz")
	d := holdLayer(m, "1.0.1", data, false, base+"/b.tgz")
	got, err := readBlob(t, m, d)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("second source not tried: %q, %v", got, err)
	}
}

// A blob held in memory is served from there even when an upstream source is
// recorded for the same digest.
func TestStreamBlobs_PrefersLocalBlob(t *testing.T) {
	data := []byte("rewritten chart")
	m, base, requests := layerUpstream(t, nil)
	d := holdLayer(m, "1.0.0", data, true, base+"/gone.tgz")
	got, err := readBlob(t, m, d)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("held blob not served: %q, %v", got, err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d upstream requests for a blob held in memory", n)
	}
}

// HEAD answers from the record alone; it must not need the upstream, which may
// be slow or gone.
func TestStreamBlobs_StatFallsBackToGet(t *testing.T) {
	data := []byte("chart layer")
	m, base, requests := layerUpstream(t, nil)
	d := holdLayer(m, "1.0.0", data, false, base+"/gone.tgz")
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/v2/"+testRepo+"/blobs/"+d.String(), nil)
	if err := m.HandleBlobs(rec, req); err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(data)) {
		t.Errorf("Content-Length = %s, want %d", got, len(data))
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d upstream requests for a HEAD", n)
	}
}

// A source that serves the wrong bytes after a 200 cannot be switched mid
// response; the next request must go to another source instead.
func TestStreamBlobs_MismatchingSourceIsTriedLast(t *testing.T) {
	data := []byte("shared tarball")
	m, base, _ := layerUpstream(t, map[string][]byte{
		"/a.tgz": []byte("republished!!!"),
		"/b.tgz": data,
	})
	d := holdLayer(m, "1.0.0", data, false, base+"/a.tgz", base+"/b.tgz")
	if _, err := readBlob(t, m, d); err == nil {
		t.Fatal("first read of the mismatching source succeeded")
	}
	if _, err := readBlob(t, m, d); err != nil {
		t.Fatalf("retry was not served by the other source: %v", err)
	}
}

// Blobs are resolved within the requested repository only, under the same key
// the manifest was stored with.
func TestBlob_OtherRepositoryIsUnknown(t *testing.T) {
	f := newStreamFixture(t)
	d := f.layer(t)
	for _, repo := range []string{f.repo, f.repo + "x", strings.TrimSuffix(f.repo, "/demo") + "/other"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/v2/"+repo+"/blobs/"+d.String(), nil)
		err := f.m.HandleBlobs(rec, req)
		if repo == f.repo {
			if err != nil {
				t.Fatalf("blob of %s not found under its own repository: %v", repo, err)
			}
			continue
		}
		if re, ok := err.(*errors.RegError); !ok || re.Status != http.StatusNotFound {
			t.Errorf("blob of %s requested under %s: got %v, want 404", f.repo, repo, err)
		}
	}
}

// MANIFEST_CACHE_TTL covers a manifest's blobs: once cleanup dropped the
// manifest, its layer is no longer served.
func TestBlob_ExpiredManifestIsUnknown(t *testing.T) {
	f := newStreamFixture(t)
	f.m.config.CacheTTL = time.Minute
	d := f.layer(t)
	f.m.mu.Lock()
	for _, r := range f.m.records[f.repo] {
		r.expires = time.Now().Add(-time.Second)
	}
	f.m.mu.Unlock()
	f.m.cleanup()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+f.repo+"/blobs/"+d.String(), nil)
	err := f.m.HandleBlobs(rec, req)
	if re, ok := err.(*errors.RegError); !ok || re.Status != http.StatusNotFound {
		t.Fatalf("blob GET after its manifest expired: got %v, want 404", err)
	}
}

// Harbor's proxy cache sends /<its path>/v2/<repo>/...; through the router such
// a request must get the same answer as the plain /v2/<repo>/... form.
func TestRouting_PrefixedPathMatchesPlain(t *testing.T) {
	f := newStreamFixture(t)
	d := f.layer(t)
	h := registry.New(f.m.Handle, f.m.HandleBlobs, f.m.HandleTags, f.m.HandleCatalog)
	host := strings.TrimSuffix(f.repo, "/demo")
	get := func(method, p string) (int, string, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, p, nil))
		return rec.Code, rec.Header().Get("Docker-Content-Digest"), rec.Body.String()
	}
	for _, tc := range []struct{ method, suffix string }{
		{http.MethodGet, "/manifests/1.0.0"},
		{http.MethodHead, "/manifests/1.0.0"},
		{http.MethodGet, "/blobs/" + d.String()},
		{http.MethodHead, "/blobs/" + d.String()},
		{http.MethodGet, "/tags/list"},
	} {
		pc, pd, pb := get(tc.method, "/v2/"+f.repo+tc.suffix)
		if pc != http.StatusOK {
			t.Fatalf("%s /v2/%s%s: status %d", tc.method, f.repo, tc.suffix, pc)
		}
		qc, qd, qb := get(tc.method, "/"+host+"/v2/"+f.repo+tc.suffix)
		if qc != pc || qd != pd || qb != pb {
			t.Errorf("%s prefixed %s: %d %s, plain: %d %s", tc.method, tc.suffix, qc, qd, pc, pd)
		}
	}
}

// /v2/v2/... leaves no repository between the last "v2" and the kind. Main
// answered such manifests 500 and blobs 404; nothing is looked up under "".
func TestHandlers_EmptyRepositoryIsNeverLookedUp(t *testing.T) {
	m := newTestManifests(t, Config{})
	data := []byte("config")
	d := digest.FromBytes(data)
	m.insert("", &record{manifest: []byte("{}"), digest: "sha256:x", config: &blob{digest: d, size: int64(len(data)), data: data}, expires: time.Now().Add(time.Hour)}, "1.0")
	for _, tc := range []struct {
		path string
		h    func(http.ResponseWriter, *http.Request) error
		want int
	}{
		{"/v2/v2/manifests/1.0", m.Handle, http.StatusInternalServerError},
		{"/v2/charts/v2/manifests/1.0", m.Handle, http.StatusInternalServerError},
		{"/v2/v2/blobs/" + d.String(), m.HandleBlobs, http.StatusNotFound},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			err := tc.h(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, tc.path, nil))
			var re *errors.RegError
			if !stderrors.As(err, &re) || re.Status != tc.want {
				t.Errorf("%s %s: got %v, want %d", method, tc.path, err, tc.want)
			}
		}
	}
}
