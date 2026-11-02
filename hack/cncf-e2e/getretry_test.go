package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Only failures that can change on repetition are retried; a 403 or an
// oversized body would only add a request and a delay.
func TestGetRetry_RetriesOnlyTransientFailures(t *testing.T) {
	retryBackoff = 0
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		limit    int64
		requests int32
	}{
		{"forbidden", http.StatusForbidden, "", 10, 1},
		{"unauthorized", http.StatusUnauthorized, "", 10, 1},
		{"not found", http.StatusNotFound, "", 10, 1},
		{"too large", http.StatusOK, "0123456789abc", 10, 1},
		{"unavailable", http.StatusServiceUnavailable, "", 10, 2},
		{"too many requests", http.StatusTooManyRequests, "", 10, 2},
	} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		if _, _, err := getRetry(srv.Client(), srv.URL, tc.limit); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
		if got := n.Load(); got != tc.requests {
			t.Errorf("%s: %d requests, want %d", tc.name, got, tc.requests)
		}
		srv.Close()
	}
}
