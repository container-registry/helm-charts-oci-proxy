package manifest

import (
	"context"
	stderrors "errors"
	"expvar"
	"io"
	"net/http"
	"sync"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/pkg/verify"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/opencontainers/go-digest"
)

var blobsStreamed = expvar.NewInt("blobs_streamed")

type layerSource struct {
	url  string
	size int64
}

// streamBlobs serves chart layers straight from their upstream URL, verified
// against the digest computed when the manifest was prepared, so tarballs are
// never held in memory. Everything else (config blobs, rewritten charts) is
// delegated to the local handler.
type streamBlobs struct {
	local   handler.BlobHandler
	open    func(ctx context.Context, url string) (io.ReadCloser, error)
	mu      sync.RWMutex
	sources map[string]layerSource
}

func newStreamBlobs(local handler.BlobHandler, open func(context.Context, string) (io.ReadCloser, error)) *streamBlobs {
	return &streamBlobs{local: local, open: open, sources: map[string]layerSource{}}
}

func (s *streamBlobs) addSource(d digest.Digest, url string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources[d.String()] = layerSource{url: url, size: size}
}

func (s *streamBlobs) source(h v1.Hash) (layerSource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.sources[h.String()]
	return src, ok
}

func (s *streamBlobs) Stat(ctx context.Context, repo string, h v1.Hash) (int64, error) {
	if src, ok := s.source(h); ok {
		return src.size, nil
	}
	if st, ok := s.local.(handler.BlobStatHandler); ok {
		return st.Stat(ctx, repo, h)
	}
	return 0, blobs.ErrNotFound
}

func (s *streamBlobs) Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error) {
	src, ok := s.source(h)
	if !ok {
		return s.local.Get(ctx, repo, h)
	}
	rc, err := s.open(ctx, src.url)
	if err != nil {
		var se *upstreamStatusError
		if stderrors.As(err, &se) && (se.status == http.StatusNotFound || se.status == http.StatusGone) {
			return nil, blobs.ErrNotFound
		}
		return nil, err
	}
	blobsStreamed.Add(1)
	// A republished upstream file fails here at EOF; the blob handler then
	// aborts the response instead of completing it.
	return verify.ReadCloser(rc, src.size, h)
}

func (s *streamBlobs) Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error {
	p, ok := s.local.(handler.BlobPutHandler)
	if !ok {
		rc.Close()
		return stderrors.New("local blob handler does not accept writes")
	}
	return p.Put(ctx, repo, h, rc)
}

func (s *streamBlobs) Delete(ctx context.Context, repo string, h v1.Hash) error {
	s.mu.Lock()
	_, had := s.sources[h.String()]
	delete(s.sources, h.String())
	s.mu.Unlock()
	d, ok := s.local.(handler.BlobDeleteHandler)
	if !ok {
		return nil
	}
	err := d.Delete(ctx, repo, h)
	if had && stderrors.Is(err, blobs.ErrNotFound) {
		return nil
	}
	return err
}
