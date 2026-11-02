package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Only failures that can change on repetition are transient. A redirect loop
// ends in the same "stopped after 10 redirects" error every time.
func TestTransient_OnlyKnownNetworkFailures(t *testing.T) {
	retryBackoff = 0

	var hops atomic.Int32
	loop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	}))
	defer loop.Close()
	c := guardUpstream(loop.Client(), true)
	if _, err := getBytes(c, loop.URL, 10); err == nil || transient(err) {
		t.Errorf("redirect loop: err %v, transient %v; want a final error", err, transient(err))
	}
	once := hops.Swap(0)
	if _, _, err := getRetry(c, loop.URL, 10); err == nil || hops.Load() != once {
		t.Errorf("redirect loop: getRetry made %d requests, one attempt makes %d", hops.Load(), once)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	if _, err := getBytes(&http.Client{}, closedURL, 10); err == nil || !transient(err) {
		t.Errorf("connection refused: err %v, transient %v; want transient", err, transient(err))
	}

	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	if _, err := getBytes(&http.Client{Timeout: 50 * time.Millisecond}, slow.URL, 10); err == nil || !transient(err) {
		t.Errorf("timeout: err %v, transient %v; want transient", err, transient(err))
	}
}
