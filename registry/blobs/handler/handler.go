package handler

import (
	"context"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"io"
)

// BlobHandler represents a minimal Blob storage backend, capable of serving
// Blob contents.
type BlobHandler interface {
	// Get gets the Blob contents, or errNotFound if the Blob wasn't found.
	Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error)
}

// BlobStatHandler is an extension interface representing a Blob storage
// backend that can serve metadata about blobs.
type BlobStatHandler interface {
	// Stat returns the size of the Blob, or errNotFound if the Blob wasn't
	// found, or redirectError if the Blob can be found elsewhere.
	Stat(ctx context.Context, repo string, h v1.Hash) (int64, error)
}

// BlobPutHandler is an extension interface representing a Blob storage backend
// that can write Blob contents.
type BlobPutHandler interface {
	// Put puts the Blob contents.
	//
	// The contents will be verified against the expected size and digest
	// as the contents are read, and an error will be returned if these
	// don't match. Implementations should return that error, or a wrapper
	// around that error, to return the correct error when these don't match.
	Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error
}

type BlobDeleteHandler interface {
	// Delete the blob contents.
	Delete(ctx context.Context, repo string, h v1.Hash) error
}
