package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// driftServer reports total packages but returns returned(n) of them on the
// n-th search, as Artifact Hub does when packages change while it is paged.
func driftServer(t *testing.T, total int, returned func(n int32) int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := searches.Add(1)
		var pkgs []Package
		for i := range returned(n) {
			p := Package{PackageID: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("c%d", i), Version: "1.0.0"}
			p.Repository.URL = "https://example.com/charts"
			pkgs = append(pkgs, p)
		}
		w.Header().Set("Pagination-Total-Count", fmt.Sprint(total))
		_ = json.NewEncoder(w).Encode(map[string]any{"packages": pkgs})
	}))
	t.Cleanup(srv.Close)
	return srv, &searches
}

// A count that drifts from Pagination-Total-Count must not abort the run: the
// discovery is repeated, and a persisting difference is kept as a warning.
func TestDiscover_ToleratesCountDrift(t *testing.T) {
	discoveryRetryDelay = 0

	srv, searches := driftServer(t, 3, func(n int32) int { return map[bool]int{true: 2, false: 3}[n == 1] })
	d, err := discover(srv.Client(), srv.URL, false)
	if err != nil || len(d.Packages) != 3 || d.Warning != "" || searches.Load() != 2 {
		t.Errorf("transient drift: err %v, %d packages, warning %q, %d searches; want 3 packages, no warning, 2 searches",
			err, len(d.Packages), d.Warning, searches.Load())
	}

	srv, searches = driftServer(t, 3, func(int32) int { return 2 })
	d, err = discover(srv.Client(), srv.URL, false)
	if err != nil || len(d.Packages) != 2 || !strings.Contains(d.Warning, "reported 3 packages") || searches.Load() != int32(discoveryAttempts) {
		t.Errorf("persisting drift: err %v, %d packages, warning %q, %d searches; want 2 packages, a warning, %d searches",
			err, len(d.Packages), d.Warning, searches.Load(), discoveryAttempts)
	}
}
