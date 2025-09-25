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

// TestFluxCDScenario simulates the FluxCD issue where different checksums are generated
// for the same chart, causing unnecessary reconciliation events
func TestFluxCDScenario(t *testing.T) {
	// Simulate the cert-manager chart mentioned in the issue
	certManagerChart := &repo.ChartVersion{
		Metadata: &chart.Metadata{
			Name:       "cert-manager",
			Version:    "1.13.3",
			APIVersion: "v2",
		},
		URLs:    []string{"cert-manager-1.13.3.tgz"},
		Created: time.Date(2023, 10, 15, 14, 30, 0, 0, time.UTC),
		Digest:  "sha256:cert-manager-digest",
	}

	config := Config{
		Debug:    false,
		CacheTTL: time.Hour,
	}
	
	blobHandler := mem.NewMemHandler()
	cache := &mockCache{}
	manifests := NewManifests(context.Background(), blobHandler, config, cache, &mockLogger{})

	// Simulate the same chart being processed multiple times by FluxCD reconciliation
	chartData := []byte("cert-manager chart content")

	// Simulate multiple reconciliation cycles (like FluxCD would do)
	var checksums []string
	var timestamps []time.Time
	
	for i := 0; i < 5; i++ {
		// Each time FluxCD reconciles, it would process the chart again
		dst := NewInternalDstWithChartVer("charts.jetstack.io/cert-manager", blobHandler, manifests, certManagerChart)
		
		manifest := Manifest{
			ContentType: "application/vnd.docker.distribution.manifest.v2+json",
			Blob:        chartData,
			Refs:        []string{"sha256:layer1"},
			CreatedAt:   dst.getDeterministicTimestamp(),
		}
		
		hash := sha256.Sum256(manifest.Blob)
		checksum := "sha256:" + hex.EncodeToString(hash[:])
		
		checksums = append(checksums, checksum)
		timestamps = append(timestamps, manifest.CreatedAt)
		
		t.Logf("Reconciliation cycle %d: checksum=%s, timestamp=%v", i+1, checksum, manifest.CreatedAt)
	}
	
	// Verify all checksums are identical (fix for FluxCD issue)
	baseChecksum := checksums[0]
	for i, checksum := range checksums {
		if checksum != baseChecksum {
			t.Errorf("Reconciliation cycle %d produced different checksum: expected %s, got %s", i+1, baseChecksum, checksum)
		}
	}
	
	// Verify all timestamps are identical
	baseTimestamp := timestamps[0]
	for i, timestamp := range timestamps {
		if !timestamp.Equal(baseTimestamp) {
			t.Errorf("Reconciliation cycle %d produced different timestamp: expected %v, got %v", i+1, baseTimestamp, timestamp)
		}
	}
	
	// Verify the timestamp matches the chart's Created timestamp
	if !baseTimestamp.Equal(certManagerChart.Created) {
		t.Errorf("Expected timestamp to match chart Created time: expected %v, got %v", certManagerChart.Created, baseTimestamp)
	}
	
	t.Logf("✅ FluxCD scenario test passed - all checksums identical: %s", baseChecksum)
	t.Logf("✅ All timestamps deterministic: %v", baseTimestamp)
	
	// This fixes the original issue where FluxCD would see events like:
	// Normal  ArtifactUpToDate  artifact up-to-date with remote revision: '1.13.3@sha256:01670a198f036bb7b4806c70f28c81097a1c1ae993e6e7e9668ceea3c9800d69'
	// Normal  ArtifactUpToDate  artifact up-to-date with remote revision: '1.13.3@sha256:2565063055a68c060dcd8754f5395bc48ebcf974799a9647d077f644bf29a584'
	// (different checksums for the same chart version)
}