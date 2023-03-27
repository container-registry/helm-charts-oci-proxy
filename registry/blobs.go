// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Returns whether this url should be handled by the Blob handler
// This is complicated because Blob is indicated by the trailing path, not the leading path.
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-a-layer
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-a-layer
func isBlob(req *http.Request) bool {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	if elem[len(elem)-1] == "" {
		elem = elem[:len(elem)-1]
	}
	if len(elem) < 3 {
		return false
	}
	return elem[len(elem)-2] == "blobs" || (elem[len(elem)-3] == "blobs" &&
		elem[len(elem)-2] == "uploads")
}

// blobHandler represents a minimal Blob storage backend, capable of serving
// Blob contents.
type blobHandler interface {
	// Get gets the Blob contents, or errNotFound if the Blob wasn't found.
	Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error)
	Debug() map[string][]byte
}

// blobStatHandler is an extension interface representing a Blob storage
// backend that can serve metadata about blobs.
type blobStatHandler interface {
	// Stat returns the size of the Blob, or errNotFound if the Blob wasn't
	// found, or redirectError if the Blob can be found elsewhere.
	Stat(ctx context.Context, repo string, h v1.Hash) (int64, error)
}

// blobPutHandler is an extension interface representing a Blob storage backend
// that can write Blob contents.
type blobPutHandler interface {
	// Put puts the Blob contents.
	//
	// The contents will be verified against the expected size and digest
	// as the contents are read, and an error will be returned if these
	// don't match. Implementations should return that error, or a wrapper
	// around that error, to return the correct error when these don't match.
	Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error
}

type blobDeleteHandler interface {
	// Delete the blob contents.
	Delete(ctx context.Context, repo string, h v1.Hash) error
}

// redirectError represents a signal that the Blob handler doesn't have the Blob
// contents, but that those contents are at another location which registry
// clients should redirect to.
type redirectError struct {
	// Location is the location to find the contents.
	Location string

	// Code is the HTTP redirect status code to return to clients.
	Code int
}

func (e redirectError) Error() string { return fmt.Sprintf("redirecting (%d): %s", e.Code, e.Location) }

// errNotFound represents an error locating the Blob.
var errNotFound = errors.New("not found")

type memHandler struct {
	m    map[string][]byte
	lock sync.Mutex
}

func (m *memHandler) Debug() map[string][]byte {
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.m
}

func (m *memHandler) Stat(_ context.Context, _ string, h v1.Hash) (int64, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	b, found := m.m[h.String()]
	if !found {
		return 0, errNotFound
	}
	return int64(len(b)), nil
}
func (m *memHandler) Get(_ context.Context, _ string, h v1.Hash) (io.ReadCloser, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	b, found := m.m[h.String()]
	if !found {
		return nil, errNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memHandler) Put(_ context.Context, _ string, h v1.Hash, rc io.ReadCloser) error {
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
func (m *memHandler) Delete(_ context.Context, _ string, h v1.Hash) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	if _, found := m.m[h.String()]; !found {
		return errNotFound
	}

	delete(m.m, h.String())
	return nil
}

// Blobs
type Blobs struct {
	BlobHandler blobHandler `json:"blobHandler"`
	registry    *registry
	// Each upload gets a unique id that writes occur to until finalized.
	// Temporary storage
	lock sync.Mutex
	log  *log.Logger
}

func (b *Blobs) handle(resp http.ResponseWriter, req *http.Request) *regError {
	ctx := req.Context()

	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	if elem[len(elem)-1] == "" {
		elem = elem[:len(elem)-1]
	}
	// Must have a path of form /v2/{name}/blobs/{upload,sha256:}
	if len(elem) < 4 {
		return &regError{
			Status:  http.StatusBadRequest,
			Code:    "NAME_INVALID",
			Message: "Blobs must be attached to a repo",
		}
	}
	target := elem[len(elem)-1]
	repo := req.URL.Host + path.Join(elem[1:len(elem)-2]...)

	switch req.Method {
	case http.MethodHead:
		h, err := v1.NewHash(target)
		if err != nil {
			return &regError{
				Status:  http.StatusBadRequest,
				Code:    "NAME_INVALID",
				Message: "invalid digest",
			}
		}

		var size int64
		if bsh, ok := b.BlobHandler.(blobStatHandler); ok {
			size, err = bsh.Stat(ctx, repo, h)
			if errors.Is(err, errNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rErr redirectError
				if errors.As(err, &rErr) {
					http.Redirect(resp, req, rErr.Location, rErr.Code)
					return nil
				}
				return regErrInternal(err)
			}
		} else {
			rc, err := b.BlobHandler.Get(ctx, repo, h)
			if errors.Is(err, errNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rErr redirectError
				if errors.As(err, &rErr) {
					http.Redirect(resp, req, rErr.Location, rErr.Code)
					return nil
				}
				return regErrInternal(err)
			}
			defer rc.Close()
			size, err = io.Copy(io.Discard, rc)
			if err != nil {
				return regErrInternal(err)
			}
		}

		resp.Header().Set("Content-Length", fmt.Sprint(size))
		resp.Header().Set("Docker-Content-Digest", h.String())
		resp.WriteHeader(http.StatusOK)
		return nil

	case http.MethodGet:
		h, err := v1.NewHash(target)
		if err != nil {
			return &regError{
				Status:  http.StatusBadRequest,
				Code:    "NAME_INVALID",
				Message: "invalid digest",
			}
		}

		var size int64
		var r io.Reader
		if bsh, ok := b.BlobHandler.(blobStatHandler); ok {
			size, err = bsh.Stat(ctx, repo, h)
			if errors.Is(err, errNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rErr redirectError
				if errors.As(err, &rErr) {
					http.Redirect(resp, req, rErr.Location, rErr.Code)
					return nil
				}
				return regErrInternal(err)
			}

			rc, err := b.BlobHandler.Get(ctx, repo, h)
			if errors.Is(err, errNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rErr redirectError
				if errors.As(err, &rErr) {
					http.Redirect(resp, req, rErr.Location, rErr.Code)
					return nil
				}

				return regErrInternal(err)
			}
			defer rc.Close()
			r = rc
		} else {
			tmp, err := b.BlobHandler.Get(ctx, repo, h)
			if errors.Is(err, errNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rerr redirectError
				if errors.As(err, &rerr) {
					http.Redirect(resp, req, rerr.Location, rerr.Code)
					return nil
				}

				return regErrInternal(err)
			}
			defer tmp.Close()
			var buf bytes.Buffer
			io.Copy(&buf, tmp)
			size = int64(buf.Len())
			r = &buf
		}

		resp.Header().Set("Content-Length", fmt.Sprint(size))
		resp.Header().Set("Docker-Content-Digest", h.String())
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, r)
		return nil

	default:
		return &regError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
}
