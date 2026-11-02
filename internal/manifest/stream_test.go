package manifest

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

type streamFixture struct {
	m         *Manifests
	repo      string
	tgz       []byte
	downloads *atomic.Int32
	serve     *atomic.Value // []byte served for the tarball
	status    atomic.Int32  // non-zero: answer the tarball with this status
	delay     atomic.Int64  // pause in the middle of the tarball body
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	f := &streamFixture{tgz: chartTarball(t, "demo", "1.0.0"), downloads: &atomic.Int32{}, serve: &atomic.Value{}}
	f.serve.Store(f.tgz)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - apiVersion: v2\n      name: demo\n      version: 1.0.0\n      urls: [%s/demo-1.0.0.tgz]\n", srv.URL)
	})
	mux.HandleFunc("/demo-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
		f.downloads.Add(1)
		if code := f.status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		data := f.serve.Load().([]byte)
		if d := time.Duration(f.delay.Load()); d > 0 {
			_, _ = w.Write(data[:len(data)/2])
			w.(http.Flusher).Flush()
			time.Sleep(d)
			data = data[len(data)/2:]
		}
		_, _ = w.Write(data)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	f.m = newTestManifests(t, Config{})
	f.m.httpClient = srv.Client()
	f.m.layerClient = newLayerClient(srv.Client())
	f.repo = strings.TrimPrefix(srv.URL, "https://") + "/demo"
	return f
}

func (f *streamFixture) manifest(t *testing.T, query string) (digest string, body []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+f.repo+"/manifests/1.0.0"+query, nil)
	if err := f.m.Handle(rec, req); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return rec.Header().Get("Docker-Content-Digest"), rec.Body.Bytes()
}

// blob answers a blob request for d in the fixture's repository.
func (f *streamFixture) blob(t *testing.T, method string, d digest.Digest) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), method, "/v2/"+f.repo+"/blobs/"+d.String(), nil)
	if err := f.m.HandleBlobs(rec, req); err != nil {
		t.Fatalf("%s blob %s: %v", method, d, err)
	}
	return rec
}

// open returns blob d as a blob GET streams it to the client.
func (f *streamFixture) open(t *testing.T, d digest.Digest) (io.ReadCloser, error) {
	t.Helper()
	b, ok := f.m.findBlob(f.repo, d)
	if !ok {
		t.Fatalf("blob %s is not held for %s", d, f.repo)
	}
	return f.m.openBlob(t.Context(), f.repo, b)
}

// The streamed path must produce the same manifest bytes as the buffered
// path; a changed digest would make every client see every chart as new.
// Dependency rewriting forces the buffered path; the test chart has no
// dependencies, so the tarball is served unchanged there.
func TestStreamedManifestMatchesBuffered(t *testing.T) {
	streamed := newStreamFixture(t)
	sd, sb := streamed.manifest(t, "")
	buffered := newStreamFixture(t)
	buffered.m.config.RewriteDependencies = true
	buffered.m.config.ProxyHost = "proxy.example.com"
	bd, bb := buffered.manifest(t, "")
	if sd != bd || !bytes.Equal(sb, bb) {
		t.Fatalf("streamed manifest %s differs from buffered %s\nstreamed: %s\nbuffered: %s", sd, bd, sb, bb)
	}
}

// The tarball is not kept after the manifest is prepared; a layer GET streams
// it from the upstream and verifies it against the manifest's digest.
func TestLayerIsStreamedNotStored(t *testing.T) {
	f := newStreamFixture(t)
	d := f.layer(t)
	if b, _ := f.m.findBlob(f.repo, d); b.data != nil {
		t.Errorf("the tarball (%d bytes) is held in memory", len(b.data))
	}
	if got := f.blob(t, http.MethodHead, d).Header().Get("Content-Length"); got != fmt.Sprint(len(f.tgz)) {
		t.Fatalf("HEAD Content-Length = %s; want %d", got, len(f.tgz))
	}
	if got := f.blob(t, http.MethodGet, d).Body.Bytes(); !bytes.Equal(got, f.tgz) {
		t.Fatalf("layer GET returned %d bytes that are not the tarball", len(got))
	}
	if n := f.downloads.Load(); n != 2 {
		t.Errorf("%d upstream downloads, want 2 (manifest, then layer)", n)
	}
}

// An upstream that republished the file under the same URL must not be served
// as the layer the manifest describes.
func TestStreamedLayerRejectsChangedUpstream(t *testing.T) {
	f := newStreamFixture(t)
	d := f.layer(t)
	f.serve.Store(chartTarball(t, "demo", "1.0.1"))
	rc, err := f.open(t, d)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("changed upstream content was served without a verification error")
	}
}
