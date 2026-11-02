package manifest

import (
	"bytes"
	"context"
	"expvar"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/container-registry/helm-charts-oci-proxy/internal/helper"
	"github.com/opencontainers/go-digest"
)

var blobsStreamed = expvar.NewInt("blobs_streamed")

var regErrBlobUnknown = &errors.RegError{
	Status:  http.StatusNotFound,
	Code:    "BLOB_UNKNOWN",
	Message: "Unknown Blob",
}

// HandleBlobs serves the config and layer blobs of the manifests held for the
// requested repository. A layer is streamed from its upstream and verified
// against the manifest's digest, so tarballs are never held in memory.
func (m *Manifests) HandleBlobs(resp http.ResponseWriter, req *http.Request) error {
	repo, _, ref, ok := helper.SplitPath(strings.TrimSuffix(req.URL.Path, "/"))
	if !ok {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "NAME_INVALID",
			Message: "Blobs must be attached to a repo",
		}
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
	d, err := digest.Parse(ref)
	if err != nil {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "NAME_INVALID",
			Message: "invalid digest",
		}
	}
	b, ok := m.findBlob(repo, d)
	if !ok {
		return regErrBlobUnknown
	}
	if req.Method == http.MethodHead {
		resp.Header().Set("Content-Length", fmt.Sprint(b.size))
		resp.Header().Set("Docker-Content-Digest", d.String())
		resp.WriteHeader(http.StatusOK)
		return nil
	}
	rc, err := m.openBlob(req.Context(), repo, b)
	if err != nil {
		return err
	}
	defer rc.Close()
	resp.Header().Set("Content-Length", fmt.Sprint(b.size))
	resp.Header().Set("Docker-Content-Digest", d.String())
	resp.WriteHeader(http.StatusOK)
	hw := &holdLastByte{w: resp}
	if _, err := io.Copy(hw, rc); err != nil {
		m.log.Printf("aborting blob %s: %v", d, err)
		// The status is already sent. Dropping the connection before the last
		// byte is the only way left to tell the client the body is bad.
		panic(http.ErrAbortHandler)
	}
	_ = hw.flush()
	return nil
}

// openBlob returns the content of b, trying every upstream URL of a layer in
// turn. Errors are *errors.RegError.
func (m *Manifests) openBlob(ctx context.Context, repo string, b blob) (io.ReadCloser, error) {
	if b.data != nil {
		return io.NopCloser(bytes.NewReader(b.data)), nil
	}
	var lastErr error
	for _, url := range b.urls {
		body, err := m.openLayer(ctx, url)
		if err != nil {
			lastErr = err
			continue
		}
		blobsStreamed.Add(1)
		if b.rewriteHost != "" {
			body = rewriteStream(body, b.rewriteHost, m.log)
		}
		// A republished upstream file fails at EOF; HandleBlobs then drops the
		// connection before the last byte instead of completing the body. The
		// status is already sent by then, so the source moves to the back and
		// the client's retry is served by the next one.
		return verifyLayer(body, b, func() { m.demote(repo, b.digest, url) }), nil
	}
	return nil, downloadError(lastErr, "BLOB_UNKNOWN")
}

// verifiedLayer reads at most the layer's size and fails at EOF unless the
// bytes read match the layer's digest.
type verifiedLayer struct {
	io.Closer
	r        io.Reader
	verifier digest.Verifier
	want     digest.Digest
	mismatch func()
}

func verifyLayer(body io.ReadCloser, b blob, mismatch func()) *verifiedLayer {
	return &verifiedLayer{Closer: body, r: io.LimitReader(body, b.size), verifier: b.digest.Verifier(), want: b.digest, mismatch: mismatch}
}

func (v *verifiedLayer) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.verifier.Write(p[:n])
	if err == io.EOF && !v.verifier.Verified() {
		v.mismatch()
		v.mismatch = func() {}
		return n, fmt.Errorf("upstream content does not match %s", v.want)
	}
	return n, err
}

// holdLastByte withholds the final byte written until flush, so a body whose
// verification fails at EOF never reaches the client complete.
type holdLastByte struct {
	w    io.Writer
	last byte
	have bool
}

func (h *holdLastByte) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if h.have {
		if _, err := h.w.Write([]byte{h.last}); err != nil {
			return 0, err
		}
	}
	if _, err := h.w.Write(p[:len(p)-1]); err != nil {
		return 0, err
	}
	h.last, h.have = p[len(p)-1], true
	return len(p), nil
}

func (h *holdLastByte) flush() error {
	if !h.have {
		return nil
	}
	h.have = false
	_, err := h.w.Write([]byte{h.last})
	return err
}
