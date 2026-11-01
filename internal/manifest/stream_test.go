package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
	f.m = NewManifests(t.Context(), mem.NewMemHandler(), Config{}, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))
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

// The streamed path must produce the same manifest bytes as the buffered
// path; a changed digest would make every client see every chart as new.
// rewrite_dependencies=true forces the buffered path; the test chart has no
// dependencies, so the tarball is served unchanged there.
func TestStreamedManifestMatchesBuffered(t *testing.T) {
	streamed := newStreamFixture(t)
	sd, sb := streamed.manifest(t, "")
	buffered := newStreamFixture(t)
	buffered.m.config.ProxyHost = "proxy.example.com"
	bd, bb := buffered.manifest(t, "?rewrite_dependencies=true")
	if sd != bd || !bytes.Equal(sb, bb) {
		t.Fatalf("streamed manifest %s differs from buffered %s\nstreamed: %s\nbuffered: %s", sd, bd, sb, bb)
	}
}

// The tarball is not kept after the manifest is prepared; a layer GET streams
// it from the upstream and verifies it against the manifest's digest.
func TestLayerIsStreamedNotStored(t *testing.T) {
	f := newStreamFixture(t)
	before := storedBlobBytes(t, f.m)
	_, body := f.manifest(t, "")
	var man ocispec.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		t.Fatal(err)
	}
	if grown := storedBlobBytes(t, f.m) - before; grown >= int64(len(f.tgz)) {
		t.Errorf("blob store grew by %d bytes, the tarball (%d bytes) is held in memory", grown, len(f.tgz))
	}

	h, err := v1.NewHash(man.Layers[0].Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	size, err := f.m.Blobs().(interface {
		Stat(context.Context, string, v1.Hash) (int64, error)
	}).Stat(t.Context(), "", h)
	if err != nil || size != int64(len(f.tgz)) {
		t.Fatalf("Stat = %d, %v; want %d", size, err, len(f.tgz))
	}
	rc, err := f.m.Blobs().Get(t.Context(), "", h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, f.tgz) {
		t.Fatalf("layer read: %v, equal=%v", err, bytes.Equal(got, f.tgz))
	}
	if n := f.downloads.Load(); n != 2 {
		t.Errorf("%d upstream downloads, want 2 (manifest, then layer)", n)
	}
}

// An upstream that republished the file under the same URL must not be served
// as the layer the manifest describes.
func TestStreamedLayerRejectsChangedUpstream(t *testing.T) {
	f := newStreamFixture(t)
	_, body := f.manifest(t, "")
	var man ocispec.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		t.Fatal(err)
	}
	f.serve.Store(chartTarball(t, "demo", "1.0.1"))
	h, _ := v1.NewHash(man.Layers[0].Digest.String())
	rc, err := f.m.Blobs().Get(t.Context(), "", h)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("changed upstream content was served without a verification error")
	}
}

// storedBlobBytes reads the in-memory blob store size published by the mem
// handler.
func storedBlobBytes(t *testing.T, _ *Manifests) int64 {
	t.Helper()
	v, ok := expvar.Get("blob_store_bytes").(*expvar.Int)
	if !ok {
		t.Fatal("blob_store_bytes expvar not published")
	}
	return v.Value()
}
