package blobs

import (
	cerrors "errors"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/registry/errors"
	"net/http"
)

var ErrNotFound = cerrors.New("not found")

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

var regErrBlobUnknown = &errors.RegError{
	Status:  http.StatusNotFound,
	Code:    "BLOB_UNKNOWN",
	Message: "Unknown Blob",
}
