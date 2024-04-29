package file

import (
	"bytes"
	"context"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"io"
	"os"
	"path"
)

type Handler struct {
	path string
}

func NewHandler(path string) *Handler {
	return &Handler{path: path}
}

func (h2 Handler) Stat(ctx context.Context, _ string, h v1.Hash) (int64, error) {
	filePath := path.Join(h2.path, h.String())

	info, err := os.Stat(filePath)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (h2 Handler) Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error {
	filePath := path.Join(h2.path, h.String())

	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, all, 0655)
}

func (h2 Handler) Delete(ctx context.Context, repo string, h v1.Hash) error {
	filePath := path.Join(h2.path, h.String())
	return os.Remove(filePath)
}

func (h2 Handler) Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error) {
	filePath := path.Join(h2.path, h.String())

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
