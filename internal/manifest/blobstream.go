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
// never held in memory. Blobs stored locally (config blobs, rewritten charts)
// are served from the local handler first.
type streamBlobs struct {
	local handler.BlobHandler
	open  func(ctx context.Context, url string) (io.ReadCloser, error)
	mu    sync.RWMutex
	// Identical tarballs published by several repositories share a digest;
	// every URL is kept so one upstream going away does not break the others.
	sources map[string][]layerSource
}

func newStreamBlobs(local handler.BlobHandler, open func(context.Context, string) (io.ReadCloser, error)) *streamBlobs {
	return &streamBlobs{local: local, open: open, sources: map[string][]layerSource{}}
}

func (s *streamBlobs) addSource(d digest.Digest, url string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range s.sources[d.String()] {
		if src.url == url {
			return
		}
	}
	s.sources[d.String()] = append(s.sources[d.String()], layerSource{url: url, size: size})
}

func (s *streamBlobs) sourcesOf(h v1.Hash) []layerSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]layerSource(nil), s.sources[h.String()]...)
}

func (s *streamBlobs) Stat(ctx context.Context, repo string, h v1.Hash) (int64, error) {
	size, err := s.localSize(ctx, repo, h)
	if !stderrors.Is(err, blobs.ErrNotFound) {
		return size, err
	}
	if srcs := s.sourcesOf(h); len(srcs) > 0 {
		return srcs[0].size, nil
	}
	return 0, blobs.ErrNotFound
}

func (s *streamBlobs) localSize(ctx context.Context, repo string, h v1.Hash) (int64, error) {
	if st, ok := s.local.(handler.BlobStatHandler); ok {
		return st.Stat(ctx, repo, h)
	}
	rc, err := s.local.Get(ctx, repo, h)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	return io.Copy(io.Discard, rc)
}

func (s *streamBlobs) Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error) {
	rc, err := s.local.Get(ctx, repo, h)
	if !stderrors.Is(err, blobs.ErrNotFound) {
		return rc, err
	}
	srcs := s.sourcesOf(h)
	if len(srcs) == 0 {
		return nil, blobs.ErrNotFound
	}
	var lastErr error
	for _, src := range srcs {
		body, err := s.open(ctx, src.url)
		if err != nil {
			lastErr = err
			continue
		}
		blobsStreamed.Add(1)
		// A republished upstream file fails at EOF; the blob handler then drops
		// the connection before the last byte instead of completing the body.
		return verify.ReadCloser(body, src.size, h)
	}
	var se *upstreamStatusError
	if stderrors.As(lastErr, &se) && (se.status == http.StatusNotFound || se.status == http.StatusGone) {
		return nil, blobs.ErrNotFound
	}
	return nil, downloadError(lastErr, "BLOB_UNKNOWN")
}

func (s *streamBlobs) Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error {
	p, ok := s.local.(handler.BlobPutHandler)
	if !ok {
		rc.Close()
		return stderrors.New("local blob handler does not accept writes")
	}
	return p.Put(ctx, repo, h, rc)
}

// Delete is only called by cleanup once no live manifest references the
// digest, so every source of it can go.
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
