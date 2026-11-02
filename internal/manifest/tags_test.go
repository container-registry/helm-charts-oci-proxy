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

// Manifests, tags and blobs share one parser, so a blob is looked up under the
// key its manifest was stored with. A "v2" segment inside the repository path
// used to cut the manifest repository short.
func TestSplitPath(t *testing.T) {
	for _, tc := range []struct{ path, repo, kind, ref string }{
		{"/v2/charts.example.com/demo/manifests/1.0.0", "charts.example.com/demo", "manifests", "1.0.0"},
		{"/v2/h/v2/demo/blobs/sha256:ab", "h/v2/demo", "blobs", "sha256:ab"},
		{"/v2/h/demo/tags/list", "h/demo", "tags", "list"},
	} {
		repo, kind, ref, ok := splitPath(tc.path)
		if !ok || repo != tc.repo || kind != tc.kind || ref != tc.ref {
			t.Errorf("splitPath(%q) = %q %q %q %v", tc.path, repo, kind, ref, ok)
		}
	}
	if _, _, _, ok := splitPath("/v2/tags/list"); ok {
		t.Error("/v2/tags/list has no repository")
	}
}
