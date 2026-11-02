package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// A cache written for another query must not be reused: -deprecated changes
// which packages are checked.
func TestLoadOrDiscover_RediscoversOnQueryChange(t *testing.T) {
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		w.Header().Set("Pagination-Total-Count", "1")
		fmt.Fprintf(w, `{"packages":[{"package_id":"p1","name":"demo","version":"1.0.0","repository":{"url":"https://example.com/charts"}}]}`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "discovery.json")
	raw, _ := json.Marshal(Discovery{Query: discoveryQuery(false), Total: 0})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if d, cached, err := loadOrDiscover(path, false, srv.Client(), srv.URL, false); err != nil || !cached || d.Query != discoveryQuery(false) {
		t.Fatalf("same query: cached=%v query=%q err=%v; want the cache", cached, d.Query, err)
	}
	d, cached, err := loadOrDiscover(path, false, srv.Client(), srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if cached || d.Query != discoveryQuery(true) || len(d.Packages) != 1 || searches.Load() != 1 {
		t.Errorf("other query: cached=%v query=%q packages=%d searches=%d; want a fresh discovery", cached, d.Query, len(d.Packages), searches.Load())
	}
}
