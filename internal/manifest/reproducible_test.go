package manifest

import (
	"os"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
)

// TestGetDeterministicCreatedTimestamp tests the function used for OCI manifest annotations
func TestGetDeterministicCreatedTimestamp(t *testing.T) {
	tests := []struct {
		name     string
		chartVer *repo.ChartVersion
		expected time.Time
	}{
		{
			name: "uses Created timestamp from index.yaml",
			chartVer: &repo.ChartVersion{
				Metadata: &chart.Metadata{
					Name:    "cert-manager",
					Version: "1.13.3",
				},
				Created: time.Date(2023, 12, 11, 14, 37, 55, 0, time.UTC),
			},
			expected: time.Date(2023, 12, 11, 14, 37, 55, 0, time.UTC),
		},
		{
			name: "fallback uses deterministic hash when Created is zero",
			chartVer: &repo.ChartVersion{
				Metadata: &chart.Metadata{
					Name:    "test-chart",
					Version: "1.0.0",
				},
				// Created is zero value
			},
			// Expected is deterministic based on hash of "test-chart@1.0.0"
			// We just verify it's deterministic, not the exact value
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result1 := getDeterministicCreatedTimestamp(tt.chartVer)
			result2 := getDeterministicCreatedTimestamp(tt.chartVer)

			// Verify determinism - same input produces same output
			if !result1.Equal(result2) {
				t.Errorf("Timestamps not deterministic: got %v and %v", result1, result2)
			}

			// If we have an expected value, verify it
			if !tt.expected.IsZero() && !result1.Equal(tt.expected) {
				t.Errorf("Expected %v, got %v", tt.expected, result1)
			}

			t.Logf("Chart %s@%s -> timestamp: %v", tt.chartVer.Name, tt.chartVer.Version, result1)
		})
	}
}

// TestOCIManifestAnnotationDeterminism verifies the OCI manifest annotation timestamp is deterministic
func TestOCIManifestAnnotationDeterminism(t *testing.T) {
	// Simulates what happens in charts.go prepareChart()
	chartVer := &repo.ChartVersion{
		Metadata: &chart.Metadata{
			Name:    "ingress-nginx",
			Version: "4.11.3",
		},
		Created: time.Date(2024, 10, 8, 21, 9, 15, 0, time.UTC),
	}

	// This is what gets set in packOpts.ManifestAnnotations
	ts1 := getDeterministicCreatedTimestamp(chartVer)
	annotation1 := ts1.Format(time.RFC3339)

	ts2 := getDeterministicCreatedTimestamp(chartVer)
	annotation2 := ts2.Format(time.RFC3339)

	if annotation1 != annotation2 {
		t.Errorf("OCI manifest annotations not deterministic: %s vs %s", annotation1, annotation2)
	}

	expected := "2024-10-08T21:09:15Z"
	if annotation1 != expected {
		t.Errorf("Expected annotation %s, got %s", expected, annotation1)
	}

	t.Logf("OCI manifest annotation 'org.opencontainers.image.created': %s", annotation1)
}

// TestDeterministicTimestampAcrossCharts verifies different charts get different but stable timestamps
func TestDeterministicTimestampAcrossCharts(t *testing.T) {
	charts := []*repo.ChartVersion{
		{Metadata: &chart.Metadata{Name: "chart-a", Version: "1.0.0"}},
		{Metadata: &chart.Metadata{Name: "chart-b", Version: "1.0.0"}},
		{Metadata: &chart.Metadata{Name: "chart-a", Version: "2.0.0"}},
	}

	timestamps := make([]time.Time, len(charts))
	for i, cv := range charts {
		timestamps[i] = getDeterministicCreatedTimestamp(cv)
		t.Logf("%s@%s -> %v", cv.Name, cv.Version, timestamps[i])
	}

	// All should be different (different chart name/version combinations)
	for i := 0; i < len(timestamps); i++ {
		for j := i + 1; j < len(timestamps); j++ {
			if timestamps[i].Equal(timestamps[j]) {
				t.Errorf("Charts %d and %d have same timestamp, expected different", i, j)
			}
		}
	}

	// But calling again should produce same results
	for i, cv := range charts {
		ts := getDeterministicCreatedTimestamp(cv)
		if !ts.Equal(timestamps[i]) {
			t.Errorf("Timestamp not stable for chart %d: got %v, expected %v", i, ts, timestamps[i])
		}
	}
}

// Mock implementations for testing
type mockCache struct{}

func (m *mockCache) Get(key any) (any, bool) {
	return nil, false
}

func (m *mockCache) SetWithTTL(key any, value any, cost int64, ttl time.Duration) bool {
	return true
}

type mockLogger struct{}

func (m *mockLogger) Printf(format string, v ...any) {}
func (m *mockLogger) Print(v ...any)                 {}
func (m *mockLogger) Println(v ...any)               {}
func (m *mockLogger) Fatal(v ...any)                 { panic(v) }
func (m *mockLogger) Fatalf(format string, v ...any) { panic(v) }
func (m *mockLogger) Fatalln(v ...any)               { panic(v) }
func (m *mockLogger) Panic(v ...any)                 { panic(v) }
func (m *mockLogger) Panicf(format string, v ...any) { panic(v) }
func (m *mockLogger) Panicln(v ...any)               { panic(v) }

// Ensure mockLogger is not unused
var _ = mockLogger{}
var _ = os.Exit
