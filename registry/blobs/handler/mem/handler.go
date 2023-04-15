package mem

import (
	"bytes"
	"context"
	"github.com/container-registry/helm-charts-oci-proxy/registry/blobs"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"io"
	"sync"
)

type Handler struct {
	m    map[string][]byte
	lock sync.Mutex
}

func NewMemHandler() *Handler {
	return &Handler{
		m: map[string][]byte{},
	}
}

func (m *Handler) Stat(_ context.Context, _ string, h v1.Hash) (int64, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	b, found := m.m[h.String()]
	if !found {
		return 0, blobs.ErrNotFound
	}
	return int64(len(b)), nil
}
func (m *Handler) Get(_ context.Context, _ string, h v1.Hash) (io.ReadCloser, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	b, found := m.m[h.String()]
	if !found {
		return nil, blobs.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *Handler) Put(_ context.Context, _ string, h v1.Hash, rc io.ReadCloser) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	m.m[h.String()] = all
	return nil
}
func (m *Handler) Delete(_ context.Context, _ string, h v1.Hash) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	if _, found := m.m[h.String()]; !found {
		return blobs.ErrNotFound
	}

	delete(m.m, h.String())
	return nil
}
