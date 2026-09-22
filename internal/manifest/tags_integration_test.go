//go:build integration

package manifest

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
)

type mapCache struct {
	m map[interface{}]interface{}
}

func (c *mapCache) SetWithTTL(key, value interface{}, _ int64, _ time.Duration) bool {
	c.m[key] = value
	return true
}

func (c *mapCache) Get(key interface{}) (interface{}, bool) {
	v, ok := c.m[key]
	return v, ok
}

// TestHandleTags_PrereleaseOnlyChart_Integration guards against a regression where
// listing tags failed with 404 for charts whose index entries only contain
// prerelease versions. Helm's IndexFile.Get uses a "*" semver constraint for an
// empty version, which never matches prereleases, so warming the chart cache
// failed and aborted the whole tag listing — breaking registry-wide scans such as
// Harbor replication.
//
// Run with: go test ./internal/manifest/ -tags integration -run TestHandleTags
func TestHandleTags_PrereleaseOnlyChart_Integration(t *testing.T) {
	m := NewManifests(context.Background(), mem.NewMemHandler(), Config{
		CacheTTL:           time.Minute,
		IndexCacheTTL:      time.Minute,
		IndexErrorCacheTTl: time.Second,
	}, &mapCache{m: map[interface{}]interface{}{}}, log.New(os.Stdout, "test-", log.LstdFlags))

	req := httptest.NewRequest(http.MethodGet, "/v2/helm.linkerd.io/edge/linkerd-failover/tags/list", nil)
	resp := httptest.NewRecorder()

	if err := m.HandleTags(resp, req); err != nil {
		t.Fatalf("HandleTags returned error: %v", err)
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.Code)
	}

	var got listTags
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.Name != "helm.linkerd.io/edge/linkerd-failover" {
		t.Errorf("unexpected name %q", got.Name)
	}
	if len(got.Tags) == 0 {
		t.Fatal("expected at least one tag")
	}
	// A single stable version would make this chart pass even without the fix.
	for _, tag := range got.Tags {
		if !strings.Contains(tag, "-") {
			t.Errorf("expected prerelease-only chart, got stable tag %q", tag)
		}
	}
	for _, want := range []string{"0.0.1-edge", "0.0.9-edge"} {
		var found bool
		for _, tag := range got.Tags {
			if tag == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tag %q missing from %v", want, got.Tags)
		}
	}
}
