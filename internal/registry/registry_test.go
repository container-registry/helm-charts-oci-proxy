package registry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Routing looks only at the trailing segments, so a prefix before /v2/ routes
// the same way, and only blob paths tolerate a trailing slash.
func TestRouting(t *testing.T) {
	var got string
	route := func(name string) Handler {
		return func(http.ResponseWriter, *http.Request) error { got = name; return nil }
	}
	h := New(route("manifests"), route("blobs"), route("tags"), route("catalog"))
	for _, tc := range []struct{ path, want string }{
		{"/v2/h/demo/manifests/1.0.0", "manifests"},
		{"/x/v2/h/demo/manifests/1.0.0", "manifests"},
		{"/v2/h/demo/manifests/1.0.0/", "404"},
		{"/v2/h/demo/tags/list", "tags"},
		{"/v2/h/demo/tags/list/", "404"},
		{"/v2/h/demo/blobs/sha256:ab", "blobs"},
		{"/v2/h/demo/blobs/sha256:ab/", "blobs"},
		{"/v2/h/demo/blobs/uploads/u", "blobs"},
		{"/v2/_catalog", "catalog"},
		{"/v2/", "v2"},
		{"/x/v2", "v2"},
	} {
		got = ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got == "" {
			got = "404"
			if rec.Code == http.StatusOK {
				got = "v2"
			}
		}
		if got != tc.want {
			t.Errorf("%s routed to %s, want %s", tc.path, got, tc.want)
		}
	}
}
