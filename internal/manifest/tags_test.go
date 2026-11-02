package manifest

import (
	goerrors "errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
)

type nopCache struct{}

func (nopCache) SetWithTTL(_, _ interface{}, _ int64, _ time.Duration) bool { return false }

func (nopCache) Get(_ interface{}) (interface{}, bool) { return nil, false }

func newTestManifests(t *testing.T, cfg Config) *Manifests {
	t.Helper()
	return NewManifests(t.Context(), cfg, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))
}

// TestHandleTags_NoChartName guards against a panic: "/v2/tags/list" passes the
// path length check but leaves no repository parts to slice.
func TestHandleTags_NoChartName(t *testing.T) {
	m := NewManifests(t.Context(), Config{}, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))

	err := m.HandleTags(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/tags/list", nil))
	if err == nil {
		t.Fatal("expected an error")
	}
	var regErr *errors.RegError
	if !goerrors.As(err, &regErr) {
		t.Fatalf("expected *errors.RegError, got %T", err)
	}
	if regErr.Status != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", regErr.Status)
	}
}

// Only GET and HEAD reach the repository check; any other method is refused
// as unknown whatever the repository looks like.
func TestHandle_OtherMethodsAreUnknown(t *testing.T) {
	m := newTestManifests(t, Config{})
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		for _, p := range []string{"/v2/mychart/manifests/1.0", "/v2/h/demo/manifests/1.0"} {
			err := m.Handle(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, p, nil))
			var re *errors.RegError
			if !goerrors.As(err, &re) || re.Status != http.StatusBadRequest || re.Code != "METHOD_UNKNOWN" {
				t.Errorf("%s %s: got %v, want 400 METHOD_UNKNOWN", method, p, err)
			}
		}
	}
	err := m.Handle(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/mychart/manifests/1.0", nil))
	var re *errors.RegError
	if !goerrors.As(err, &re) || re.Status != http.StatusInternalServerError {
		t.Errorf("GET /v2/mychart/manifests/1.0: got %v, want 500", err)
	}
}
