package main

import (
	"strings"
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

// A text body whose 2048-byte limit falls inside a multi-byte rune is still text.
func TestPrintable_TruncatesAtRuneBoundary(t *testing.T) {
	body := []byte(strings.Repeat("a", 2047) + strings.Repeat("ä", 10))
	got := printable(body)
	if strings.HasPrefix(got, "<") || len(got) != 2047 {
		t.Errorf("printable returned %d bytes starting %q, want the 2047-byte text prefix", len(got), got[:min(len(got), 20)])
	}
}

// A -target without the docker: prefix was ignored without a word.
func TestValidTarget(t *testing.T) {
	for target, ok := range map[string]bool{"": true, "docker:chartproxy": true, "chartproxy": false, "docker:": false, "1234": false} {
		if err := validTarget(target); (err == nil) != ok {
			t.Errorf("validTarget(%q) = %v, want ok=%v", target, err, ok)
		}
	}
}
