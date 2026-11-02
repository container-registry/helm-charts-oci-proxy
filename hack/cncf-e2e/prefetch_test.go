package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// -all-versions needs every index before the first check; fetching them one
// after another made startup as slow as the sum of all index downloads.
func TestBuildJobs_PrefetchesIndexesInParallel(t *testing.T) {
	for _, tc := range []struct{ perHost, minPeak, maxPeak int32 }{{8, 2, 3}, {1, 1, 1}} {
		if p := prefetchPeak(t, tc.perHost); p < tc.minPeak || p > tc.maxPeak {
			t.Errorf("-per-host %d: peak %d concurrent index fetches to one host, want %d..%d", tc.perHost, p, tc.minPeak, tc.maxPeak)
		}
	}
}

// prefetchPeak runs an -all-versions prefetch of 8 repositories on one host
// with -concurrency 3 and returns the peak number of index fetches in flight.
func prefetchPeak(t *testing.T, perHost int32) int32 {
	t.Helper()
	var inflight, peak atomic.Int32
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, "apiVersion: v1\nentries: {}\n")
	}))
	defer up.Close()
	pu, _ := url.Parse("http://127.0.0.1:1")
	k := newChecker(pu, up.Client(), guardUpstream(up.Client(), true), "helm", t.TempDir(), 0, 0)

	var pkgs []Package
	for i := range 8 {
		p := Package{Name: "demo", Version: "1.0.0"}
		p.Repository.URL = fmt.Sprintf("%s/r%d", up.URL, i)
		pkgs = append(pkgs, p)
	}
	jobs := buildJobs(k, pkgs, true, nil, 3, int(perHost))
	if len(jobs) != 8 {
		t.Fatalf("%d jobs, want 8", len(jobs))
	}
	return peak.Load()
}
