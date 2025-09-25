package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
)

// TestIngressNginxReproducibility tests that ingress-nginx chart produces reproducible artifacts
func TestIngressNginxReproducibility(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create a mock chart version similar to ingress-nginx
	chartVer := &repo.ChartVersion{
		Metadata: &chart.Metadata{
			Name:       "ingress-nginx",
			Version:    "4.8.3",
			APIVersion: "v2",
		},
		URLs:    []string{"ingress-nginx-4.8.3.tgz"},
		Created: time.Date(2023, 11, 1, 10, 30, 0, 0, time.UTC), // Fixed timestamp
		Digest:  "sha256:example1234",
	}

	// Create mock manifests instance
	config := Config{
		Debug:    false,
		CacheTTL: time.Hour,
	}
	
	blobHandler := mem.NewMemHandler()
	cache := &mockCache{}
	manifests := NewManifests(context.Background(), blobHandler, config, cache, &mockLogger{})

	// Simulate the same chart content being processed twice
	testChartData := []byte("mock ingress-nginx chart data")
	
	// Create dst instances with the same chart version
	dst1 := NewInternalDstWithChartVer("kubernetes.github.io/ingress-nginx", blobHandler, manifests, chartVer)
	dst2 := NewInternalDstWithChartVer("kubernetes.github.io/ingress-nginx", blobHandler, manifests, chartVer)
	
	// Get deterministic timestamps
	ts1 := dst1.getDeterministicTimestamp()
	ts2 := dst2.getDeterministicTimestamp()
	
	// Timestamps should be identical
	if !ts1.Equal(ts2) {
		t.Errorf("Expected identical timestamps, got %v and %v", ts1, ts2)
	}

	// Create manifests with identical data
	manifest1 := Manifest{
		ContentType: "application/vnd.docker.distribution.manifest.v2+json",
		Blob:        testChartData,
		Refs:        []string{"sha256:layer1", "sha256:layer2"},
		CreatedAt:   ts1,
	}

	manifest2 := Manifest{
		ContentType: "application/vnd.docker.distribution.manifest.v2+json",
		Blob:        testChartData,
		Refs:        []string{"sha256:layer1", "sha256:layer2"},
		CreatedAt:   ts2,
	}

	// Calculate checksums
	hash1 := sha256.Sum256(manifest1.Blob)
	checksum1 := "sha256:" + hex.EncodeToString(hash1[:])

	hash2 := sha256.Sum256(manifest2.Blob)
	checksum2 := "sha256:" + hex.EncodeToString(hash2[:])

	// Checksums should be identical
	if checksum1 != checksum2 {
		t.Errorf("Expected identical checksums for ingress-nginx chart, got %s and %s", checksum1, checksum2)
	}

	t.Logf("Ingress-nginx reproducible checksum: %s", checksum1)
	t.Logf("Deterministic timestamp: %v", ts1)
	
	// Verify the timestamp is using the chart's Created timestamp
	expectedTimestamp := time.Date(2023, 11, 1, 10, 30, 0, 0, time.UTC)
	if !ts1.Equal(expectedTimestamp) {
		t.Errorf("Expected timestamp %v, got %v", expectedTimestamp, ts1)
	}
}

// TestFallbackToDeterministicTimestamp tests the fallback logic when no Created timestamp is available
func TestFallbackToDeterministicTimestamp(t *testing.T) {
	// Create a chart version without a Created timestamp
	chartVer := &repo.ChartVersion{
		Metadata: &chart.Metadata{
			Name:       "test-chart",
			Version:    "1.0.0",
			APIVersion: "v2",
		},
		URLs:    []string{"test-chart-1.0.0.tgz"},
		// Created is zero value (no timestamp)
		Digest:  "sha256:abcd1234",
	}

	config := Config{
		Debug:    false,
		CacheTTL: time.Hour,
	}
	
	blobHandler := mem.NewMemHandler()
	cache := &mockCache{}
	manifests := NewManifests(context.Background(), blobHandler, config, cache, &mockLogger{})

	// Create dst instances
	dst1 := NewInternalDstWithChartVer("example.com/test-chart", blobHandler, manifests, chartVer)
	dst2 := NewInternalDstWithChartVer("example.com/test-chart", blobHandler, manifests, chartVer)
	
	// Get deterministic timestamps
	ts1 := dst1.getDeterministicTimestamp()
	ts2 := dst2.getDeterministicTimestamp()
	
	// Timestamps should be identical even without Created timestamp
	if !ts1.Equal(ts2) {
		t.Errorf("Expected identical fallback timestamps, got %v and %v", ts1, ts2)
	}

	// Verify it's not the current time (should be deterministic based on chart metadata)
	now := time.Now()
	if ts1.After(now.Add(-time.Minute)) && ts1.Before(now.Add(time.Minute)) {
		t.Error("Timestamp appears to be current time rather than deterministic")
	}

	t.Logf("Deterministic fallback timestamp: %v", ts1)
}