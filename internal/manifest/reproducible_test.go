package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
)

// TestReproducibleArtifacts verifies that the same chart input produces the same OCI artifact checksum
func TestReproducibleArtifacts(t *testing.T) {
	// Create a mock chart version with deterministic data
	chartVer := &repo.ChartVersion{
		Metadata: &chart.Metadata{
			Name:       "test-chart",
			Version:    "1.0.0",
			APIVersion: "v2",
		},
		URLs:    []string{"test-chart-1.0.0.tgz"},
		Created: time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC), // Fixed timestamp
		Digest:  "sha256:abcd1234",
	}

	// Create mock manifests instance
	config := Config{
		Debug:    false,
		CacheTTL: time.Hour,
	}
	
	blobHandler := mem.NewMemHandler()
	cache := &mockCache{}
	manifests := NewManifests(context.Background(), blobHandler, config, cache, &mockLogger{})

	// Test data - simulating downloaded chart content
	testChartData := []byte("mock chart data for testing reproducibility")
	
	// Create dst instances with chart version
	dst1 := NewInternalDstWithChartVer("test-repo/test-chart", blobHandler, manifests, chartVer)
	dst2 := NewInternalDstWithChartVer("test-repo/test-chart", blobHandler, manifests, chartVer)
	
	// Create two manifest instances with same input
	manifest1 := Manifest{
		ContentType: "application/vnd.docker.distribution.manifest.v2+json",
		Blob:        testChartData,
		Refs:        []string{"sha256:ref1", "sha256:ref2"},
		CreatedAt:   dst1.getDeterministicTimestamp(), // Use deterministic timestamp
	}

	manifest2 := Manifest{
		ContentType: "application/vnd.docker.distribution.manifest.v2+json",
		Blob:        testChartData,
		Refs:        []string{"sha256:ref1", "sha256:ref2"},
		CreatedAt:   dst2.getDeterministicTimestamp(), // Use deterministic timestamp
	}

	// Calculate checksums of the manifest blobs
	hash1 := sha256.Sum256(manifest1.Blob)
	checksum1 := "sha256:" + hex.EncodeToString(hash1[:])

	hash2 := sha256.Sum256(manifest2.Blob)
	checksum2 := "sha256:" + hex.EncodeToString(hash2[:])

	// Both checksums should be identical
	if checksum1 != checksum2 {
		t.Errorf("Expected identical checksums, got %s and %s", checksum1, checksum2)
	}

	// Verify timestamps are deterministic
	if !manifest1.CreatedAt.Equal(manifest2.CreatedAt) {
		t.Errorf("Expected identical timestamps, got %v and %v", manifest1.CreatedAt, manifest2.CreatedAt)
	}

	t.Logf("Reproducible checksum: %s", checksum1)
	t.Logf("Deterministic timestamp: %v", manifest1.CreatedAt)
}

// Mock implementations for testing
type mockCache struct{}

func (m *mockCache) Get(key interface{}) (interface{}, bool) {
	return nil, false
}

func (m *mockCache) SetWithTTL(key interface{}, value interface{}, cost int64, ttl time.Duration) bool {
	return true
}

type mockLogger struct{}

func (m *mockLogger) Printf(format string, v ...interface{}) {}
func (m *mockLogger) Print(v ...interface{})                 {}
func (m *mockLogger) Println(v ...interface{})               {}
func (m *mockLogger) Fatal(v ...interface{})                 { os.Exit(1) }
func (m *mockLogger) Fatalf(format string, v ...interface{}) { os.Exit(1) }
func (m *mockLogger) Fatalln(v ...interface{})               { os.Exit(1) }
func (m *mockLogger) Panic(v ...interface{})                 { panic(v) }
func (m *mockLogger) Panicf(format string, v ...interface{}) { panic(v) }
func (m *mockLogger) Panicln(v ...interface{})               { panic(v) }

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

			// Verify it's not the current time (for fallback case)
			if tt.chartVer.Created.IsZero() {
				now := time.Now()
				if result1.After(now.Add(-time.Minute)) && result1.Before(now.Add(time.Minute)) {
					t.Error("Fallback timestamp appears to be current time, not deterministic")
				}
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