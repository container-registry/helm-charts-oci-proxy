package blobs

import (
	"bytes"
	cerrors "errors"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/registry/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/registry/errors"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sirupsen/logrus"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
)

// errNotFound represents an error locating the Blob.

// Blobs service
type Blobs struct {
	handler handler.BlobHandler `json:"blobHandler"`
	// Each upload gets a unique id that writes occur to until finalized.
	// Temporary storage
	lock sync.Mutex
	log  logrus.StdLogger
}

func NewBlobs(blobHandler handler.BlobHandler, log logrus.StdLogger) *Blobs {
	return &Blobs{handler: blobHandler, log: log}
}

func (b *Blobs) Handle(resp http.ResponseWriter, req *http.Request) error {
	ctx := req.Context()

	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	if elem[len(elem)-1] == "" {
		elem = elem[:len(elem)-1]
	}
	// Must have a path of form /v2/{name}/blobs/{upload,sha256:}
	if len(elem) < 4 {
		return &errors.RegError{
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
			return &errors.RegError{
				Status:  http.StatusBadRequest,
				Code:    "NAME_INVALID",
				Message: "invalid digest",
			}
		}

		var size int64
		if bsh, ok := b.handler.(handler.BlobStatHandler); ok {
			size, err = bsh.Stat(ctx, repo, h)
			if cerrors.Is(err, ErrNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rErr redirectError
				if cerrors.As(err, &rErr) {
					http.Redirect(resp, req, rErr.Location, rErr.Code)
					return nil
				}
				return errors.RegErrInternal(err)
			}
		} else {
			rc, err := b.handler.Get(ctx, repo, h)
			if cerrors.Is(err, ErrNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rErr redirectError
				if cerrors.As(err, &rErr) {
					http.Redirect(resp, req, rErr.Location, rErr.Code)
					return nil
				}
				return errors.RegErrInternal(err)
			}
			defer rc.Close()
			size, err = io.Copy(io.Discard, rc)
			if err != nil {
				return errors.RegErrInternal(err)
			}
		}

		resp.Header().Set("Content-Length", fmt.Sprint(size))
		resp.Header().Set("Docker-Content-Digest", h.String())
		resp.WriteHeader(http.StatusOK)
		return nil

	case http.MethodGet:
		h, err := v1.NewHash(target)
		if err != nil {
			return &errors.RegError{
				Status:  http.StatusBadRequest,
				Code:    "NAME_INVALID",
				Message: "invalid digest",
			}
		}

		var size int64
		var r io.Reader
		if bsh, ok := b.handler.(handler.BlobStatHandler); ok {
			size, err = bsh.Stat(ctx, repo, h)
			if cerrors.Is(err, ErrNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rErr redirectError
				if cerrors.As(err, &rErr) {
					http.Redirect(resp, req, rErr.Location, rErr.Code)
					return nil
				}
				return errors.RegErrInternal(err)
			}

			rc, err := b.handler.Get(ctx, repo, h)
			if cerrors.Is(err, ErrNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rErr redirectError
				if cerrors.As(err, &rErr) {
					http.Redirect(resp, req, rErr.Location, rErr.Code)
					return nil
				}

				return errors.RegErrInternal(err)
			}
			defer rc.Close()
			r = rc
		} else {
			tmp, err := b.handler.Get(ctx, repo, h)
			if cerrors.Is(err, ErrNotFound) {
				return regErrBlobUnknown
			} else if err != nil {
				var rerr redirectError
				if cerrors.As(err, &rerr) {
					http.Redirect(resp, req, rerr.Location, rerr.Code)
					return nil
				}

				return errors.RegErrInternal(err)
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
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
}
