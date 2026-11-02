package main

import (
	"strings"
	"testing"
)

// Peaks that were never sampled must not read as 0.0 MiB measurements.
func TestMemSummary_UnmeasuredIsNotZero(t *testing.T) {
	if s := (&memStats{}).summary(); !strings.Contains(s, "not measured") || strings.Contains(s, "0.0 MiB") {
		t.Errorf("summary %q, want \"not measured\" and no 0.0 MiB", s)
	}
	s := (&memStats{Samples: 3, PeakHeapInuseMiB: 12.5}).summary()
	if !strings.Contains(s, "peak Go heap in use 12.5 MiB") || strings.Contains(s, "working set") || strings.Contains(s, "not measured") {
		t.Errorf("summary %q, want only the Go heap figure", s)
	}
}

// A failed OOMKilled read must be recorded; without it the summary leaves out
// the OOM line, which reads as "not killed".
func TestMemFinish_RecordsInspectFailure(t *testing.T) {
	m := &memStats{}
	m.finish("docker:cncf-e2e-test-no-such-container")
	if m.OOMKilled != nil || len(m.Errors) != 2 || !strings.HasPrefix(m.Errors[1], "OOMKilled: ") {
		t.Errorf("OOMKilled %v, errors %q; want nil and a memory.peak and an OOMKilled error", m.OOMKilled, m.Errors)
	}
}
