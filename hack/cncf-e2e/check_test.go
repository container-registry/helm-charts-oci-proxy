package main

import (
	"testing"
	"time"
)

func TestCheck_RecordsDuration(t *testing.T) {
	p := Package{Name: "demo", Version: "1.0.0"}
	p.Repository.URL = "oci://example.com/charts"
	r := (&checker{}).check(job{pkg: p, version: p.Version})
	if r.Class != classNA || r.Seconds <= 0 {
		t.Errorf("class %s, seconds %v; want %s and a positive duration", r.Class, r.Seconds, classNA)
	}
}

// A retry inside the proxy's error-cache TTL reads back the cached failure,
// so a transient upstream error would end as proxy-bug.
func TestDefaultRetryDelay_OutlastsProxyErrorCache(t *testing.T) {
	const proxyDefaultErrorTTL = 30 * time.Second // cmd/serve.go INDEX_ERROR_CACHE_TTL
	if defaultRetryDelay <= proxyDefaultErrorTTL {
		t.Errorf("defaultRetryDelay %s, want more than %s", defaultRetryDelay, proxyDefaultErrorTTL)
	}
}
