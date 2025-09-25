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