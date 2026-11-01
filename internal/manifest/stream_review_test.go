package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	stderrors "errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	v1 "github.com/google/go-containerregistry/pkg/v1"
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

func (f *streamFixture) layer(t *testing.T) v1.Hash {
	t.Helper()
	_, body := f.manifest(t, "")
	var man ocispec.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		t.Fatal(err)
	}
	h, err := v1.NewHash(man.Layers[0].Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	return h
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
	h := f.layer(t)
	f.m.httpClient = &http.Client{Transport: f.m.httpClient.Transport, Timeout: 100 * time.Millisecond}
	f.m.layerClient = newLayerClient(f.m.httpClient)
	f.delay.Store(int64(300 * time.Millisecond))
	rc, err := f.m.Blobs().Get(t.Context(), "", h)
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
	h := f.layer(t)
	changed := bytes.Clone(f.tgz)
	changed[len(changed)-1] ^= 0xff
	f.serve.Store(changed)

	b := blobs.NewBlobs(f.m.Blobs(), log.New(os.Stdout, "test-", log.LstdFlags))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = b.Handle(w, r)
	}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/v2/" + f.repo + "/blobs/" + h.String())
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
	h := f.layer(t)
	f.status.Store(http.StatusServiceUnavailable)
	_, err := f.m.Blobs().Get(t.Context(), "", h)
	var re *errors.RegError
	if !stderrors.As(err, &re) || re.Status != http.StatusBadGateway {
		t.Fatalf("got %v, want a 502 RegError", err)
	}
	f.status.Store(http.StatusNotFound)
	if _, err := f.m.Blobs().Get(t.Context(), "", h); !stderrors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("upstream 404: got %v, want ErrNotFound", err)
	}
}

func digestOf(h v1.Hash) digest.Digest { return digest.Digest(h.String()) }

func fakeOpen(bodies map[string][]byte) func(context.Context, string) (io.ReadCloser, error) {
	return func(_ context.Context, url string) (io.ReadCloser, error) {
		if b, ok := bodies[url]; ok {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
		return nil, &upstreamStatusError{url: url, status: http.StatusNotFound}
	}
}

// Two repositories publishing the same tarball share its digest; losing one
// upstream must not break the other's layer.
func TestStreamBlobs_FallsBackToOtherSources(t *testing.T) {
	data := []byte("shared tarball")
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	s := newStreamBlobs(mem.NewMemHandler(), fakeOpen(map[string][]byte{"https://b/x.tgz": data}))
	d := digestOf(h)
	s.addSource(d, "https://a/x.tgz", int64(len(data)))
	s.addSource(d, "https://b/x.tgz", int64(len(data)))
	rc, err := s.Get(t.Context(), "", h)
	if err != nil {
		t.Fatalf("second source not tried: %v", err)
	}
	defer rc.Close()
	if got, err := io.ReadAll(rc); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %q, %v", got, err)
	}
}

// A blob stored locally is served from memory even when an upstream source is
// recorded for the same digest.
func TestStreamBlobs_PrefersLocalBlob(t *testing.T) {
	data := []byte("rewritten chart")
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	local := mem.NewMemHandler()
	if err := local.Put(t.Context(), "", h, io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatal(err)
	}
	s := newStreamBlobs(local, fakeOpen(nil))
	s.addSource(digestOf(h), "https://gone/x.tgz", int64(len(data)))
	rc, err := s.Get(t.Context(), "", h)
	if err != nil {
		t.Fatalf("local blob not served: %v", err)
	}
	rc.Close()
}

type getOnly struct{ m *mem.Handler }

func (g getOnly) Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error) {
	return g.m.Get(ctx, repo, h)
}

// handler.BlobStatHandler is optional; a backend with only Get must still
// answer HEAD.
func TestStreamBlobs_StatFallsBackToGet(t *testing.T) {
	data := []byte("config")
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	m := mem.NewMemHandler()
	if err := m.Put(t.Context(), "", h, io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatal(err)
	}
	s := newStreamBlobs(getOnly{m}, fakeOpen(nil))
	if size, err := s.Stat(t.Context(), "", h); err != nil || size != int64(len(data)) {
		t.Fatalf("Stat = %d, %v; want %d", size, err, len(data))
	}
}
