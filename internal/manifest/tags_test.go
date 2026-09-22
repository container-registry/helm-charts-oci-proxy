package manifest

import (
	"context"
	goerrors "errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
)

type nopCache struct{}

func (nopCache) SetWithTTL(_, _ interface{}, _ int64, _ time.Duration) bool { return false }

func (nopCache) Get(_ interface{}) (interface{}, bool) { return nil, false }

// TestHandleTags_NoChartName guards against a panic: "/v2/tags/list" passes the
// path length check but leaves no repository parts to slice.
func TestHandleTags_NoChartName(t *testing.T) {
	m := NewManifests(context.Background(), mem.NewMemHandler(), Config{}, nopCache{}, log.New(os.Stdout, "test-", log.LstdFlags))

	err := m.HandleTags(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v2/tags/list", nil))
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
