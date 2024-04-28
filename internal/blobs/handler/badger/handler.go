package badger

import (
	"bytes"
	"context"
	"github.com/dgraph-io/badger/v3"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"io"
)

type Handler struct {
	db *badger.DB
}

func (h2 Handler) Stat(ctx context.Context, _ string, h v1.Hash) (int64, error) {
	var size int64
	err := h2.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(h.String()))
		if err != nil {
			return err
		}
		size = item.ValueSize()
		return nil
	})

	return size, err
}

func (h2 Handler) Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error {
	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	return h2.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(h.String()), all)
	})
}

func (h2 Handler) Delete(ctx context.Context, repo string, h v1.Hash) error {
	return h2.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(h.String()))
	})
}

func (h2 Handler) Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error) {
	var data []byte

	err := h2.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(h.String()))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			data = val
			return nil
		})
	})

	return io.NopCloser(bytes.NewReader(data)), err
}

func NewHandler(db *badger.DB) *Handler {
	return &Handler{db: db}
}
