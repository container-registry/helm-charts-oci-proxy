package main

import (
	"testing"
)

func TestCheck_RecordsDuration(t *testing.T) {
	p := Package{Name: "demo", Version: "1.0.0"}
	p.Repository.URL = "oci://example.com/charts"
	r := (&checker{}).check(job{pkg: p, version: p.Version})
	if r.Class != classNA || r.Seconds <= 0 {
		t.Errorf("class %s, seconds %v; want %s and a positive duration", r.Class, r.Seconds, classNA)
	}
}
