package manifest

import (
	"os"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

// TestGenerateOCIAnnotations verifies the generateOCIAnnotations helper function
func TestGenerateOCIAnnotations(t *testing.T) {
	fixedTime := time.Date(2025, 12, 15, 17, 31, 44, 0, time.UTC)
	createdStr := fixedTime.Format(time.RFC3339)

	t.Run("nil metadata returns only created annotation", func(t *testing.T) {
		got := generateOCIAnnotations(nil, fixedTime)
		if len(got) != 1 {
			t.Fatalf("expected 1 annotation, got %d: %v", len(got), got)
		}
		if got[ocispec.AnnotationCreated] != createdStr {
			t.Errorf("expected created %s, got %s", createdStr, got[ocispec.AnnotationCreated])
		}
	})

	t.Run("minimal chart with only name and version", func(t *testing.T) {
		meta := &chart.Metadata{Name: "mychart", Version: "1.2.3"}
		got := generateOCIAnnotations(meta, fixedTime)
		assertEqual(t, got, ocispec.AnnotationTitle, "mychart")
		assertEqual(t, got, ocispec.AnnotationVersion, "1.2.3")
		assertEqual(t, got, ocispec.AnnotationCreated, createdStr)
		assertAbsent(t, got, ocispec.AnnotationDescription)
		assertAbsent(t, got, ocispec.AnnotationURL)
		assertAbsent(t, got, ocispec.AnnotationSource)
		assertAbsent(t, got, ocispec.AnnotationAuthors)
	})

	t.Run("chart with all fields", func(t *testing.T) {
		meta := &chart.Metadata{
			Name:        "harbor",
			Version:     "1.18.1",
			Description: "An open source trusted cloud native registry",
			Home:        "https://goharbor.io",
			Sources:     []string{"https://github.com/goharbor/harbor", "https://github.com/goharbor/other"},
			Maintainers: []*chart.Maintainer{
				{Name: "Yan Wang", Email: "yan@example.com"},
				{Name: "Stone Zhang", Email: "stone@example.com"},
			},
		}
		got := generateOCIAnnotations(meta, fixedTime)
		assertEqual(t, got, ocispec.AnnotationTitle, "harbor")
		assertEqual(t, got, ocispec.AnnotationVersion, "1.18.1")
		assertEqual(t, got, ocispec.AnnotationDescription, "An open source trusted cloud native registry")
		assertEqual(t, got, ocispec.AnnotationURL, "https://goharbor.io")
		// Only Sources[0] should be used
		assertEqual(t, got, ocispec.AnnotationSource, "https://github.com/goharbor/harbor")
		assertEqual(t, got, ocispec.AnnotationAuthors, "Yan Wang (yan@example.com), Stone Zhang (stone@example.com)")
	})

	t.Run("maintainer without email uses name only", func(t *testing.T) {
		meta := &chart.Metadata{
			Name:        "mychart",
			Version:     "1.0.0",
			Maintainers: []*chart.Maintainer{{Name: "Alice"}, {Name: "Bob", Email: "bob@example.com"}},
		}
		got := generateOCIAnnotations(meta, fixedTime)
		assertEqual(t, got, ocispec.AnnotationAuthors, "Alice, Bob (bob@example.com)")
	})

	t.Run("custom annotations are copied", func(t *testing.T) {
		meta := &chart.Metadata{
			Name:        "mychart",
			Version:     "1.0.0",
			Annotations: map[string]string{"artifacthub.io/changes": "fix bug"},
		}
		got := generateOCIAnnotations(meta, fixedTime)
		assertEqual(t, got, "artifacthub.io/changes", "fix bug")
	})

	t.Run("custom annotations cannot override title or version", func(t *testing.T) {
		meta := &chart.Metadata{
			Name:    "mychart",
			Version: "1.0.0",
			Annotations: map[string]string{
				ocispec.AnnotationTitle:   "hacked-title",
				ocispec.AnnotationVersion: "hacked-version",
			},
		}
		got := generateOCIAnnotations(meta, fixedTime)
		assertEqual(t, got, ocispec.AnnotationTitle, "mychart")
		assertEqual(t, got, ocispec.AnnotationVersion, "1.0.0")
	})

	t.Run("custom annotations can override non-protected standard annotations", func(t *testing.T) {
		meta := &chart.Metadata{
			Name:        "mychart",
			Version:     "1.0.0",
			Description: "standard description",
			Home:        "https://standard.example.com",
			Annotations: map[string]string{
				ocispec.AnnotationDescription: "custom description",
				ocispec.AnnotationURL:         "https://custom.example.com",
			},
		}
		got := generateOCIAnnotations(meta, fixedTime)
		// Custom annotations override description and url (matching Helm behavior)
		assertEqual(t, got, ocispec.AnnotationDescription, "custom description")
		assertEqual(t, got, ocispec.AnnotationURL, "https://custom.example.com")
		// But title and version still come from metadata fields
		assertEqual(t, got, ocispec.AnnotationTitle, "mychart")
		assertEqual(t, got, ocispec.AnnotationVersion, "1.0.0")
	})
}

// TestGenerateOCIAnnotations_RealCharts verifies annotation output against
// real-world chart metadata from Harbor, ingress-nginx, and cert-manager.
func TestGenerateOCIAnnotations_RealCharts(t *testing.T) {
	fixedTime := time.Date(2025, 12, 15, 17, 31, 44, 0, time.UTC)
	createdStr := fixedTime.Format(time.RFC3339)

	tests := []struct {
		name     string
		meta     *chart.Metadata
		expected map[string]string
		absent   []string
	}{
		{
			name: "harbor",
			meta: &chart.Metadata{
				Name:        "harbor",
				Version:     "1.18.2",
				Description: "An open source trusted cloud native registry that stores, signs, and scans content",
				Home:        "https://goharbor.io",
				Sources:     []string{"https://github.com/goharbor/harbor", "https://github.com/goharbor/harbor-helm"},
				Maintainers: []*chart.Maintainer{
					{Name: "Yan Wang", Email: "yan-yw.wang@broadcom.com"},
					{Name: "Stone Zhang", Email: "stone.zhang@broadcom.com"},
					{Name: "Miner Yang", Email: "miner.yang@broadcom.com"},
				},
			},
			expected: map[string]string{
				ocispec.AnnotationTitle:       "harbor",
				ocispec.AnnotationVersion:     "1.18.2",
				ocispec.AnnotationDescription: "An open source trusted cloud native registry that stores, signs, and scans content",
				ocispec.AnnotationURL:         "https://goharbor.io",
				ocispec.AnnotationSource:      "https://github.com/goharbor/harbor",
				ocispec.AnnotationAuthors:     "Yan Wang (yan-yw.wang@broadcom.com), Stone Zhang (stone.zhang@broadcom.com), Miner Yang (miner.yang@broadcom.com)",
				ocispec.AnnotationCreated:     createdStr,
			},
		},
		{
			name: "ingress-nginx",
			meta: &chart.Metadata{
				Name:        "ingress-nginx",
				Version:     "4.14.3",
				Description: "Ingress controller for Kubernetes using NGINX as a reverse proxy and load balancer",
				Home:        "https://github.com/kubernetes/ingress-nginx",
				Sources:     []string{"https://github.com/kubernetes/ingress-nginx"},
				Maintainers: []*chart.Maintainer{
					{Name: "cpanato"},
					{Name: "Gacko"},
					{Name: "strongjz"},
					{Name: "tao12345666333"},
				},
				Annotations: map[string]string{
					"artifacthub.io/changes":    "- Update Ingress-Nginx version controller-v1.14.3\n",
					"artifacthub.io/prerelease": "false",
				},
			},
			expected: map[string]string{
				ocispec.AnnotationTitle:       "ingress-nginx",
				ocispec.AnnotationVersion:     "4.14.3",
				ocispec.AnnotationDescription: "Ingress controller for Kubernetes using NGINX as a reverse proxy and load balancer",
				ocispec.AnnotationURL:         "https://github.com/kubernetes/ingress-nginx",
				ocispec.AnnotationSource:      "https://github.com/kubernetes/ingress-nginx",
				ocispec.AnnotationAuthors:     "cpanato, Gacko, strongjz, tao12345666333",
				ocispec.AnnotationCreated:     createdStr,
				"artifacthub.io/changes":      "- Update Ingress-Nginx version controller-v1.14.3\n",
				"artifacthub.io/prerelease":   "false",
			},
		},
		{
			name: "cert-manager",
			meta: &chart.Metadata{
				Name:        "cert-manager",
				Version:     "v1.19.3",
				Description: "A Helm chart for cert-manager",
				Home:        "https://cert-manager.io",
				Sources:     []string{"https://github.com/cert-manager/cert-manager"},
				Maintainers: []*chart.Maintainer{
					{Name: "cert-manager-maintainers", Email: "cert-manager-maintainers@googlegroups.com"},
				},
				Annotations: map[string]string{
					"artifacthub.io/category":   "security",
					"artifacthub.io/license":    "Apache-2.0",
					"artifacthub.io/prerelease": "false",
					"artifacthub.io/signKey":    "fingerprint: 1020CF3C033D4F35BAE1C19E1226061C665DF13E\nurl: https://cert-manager.io/public-keys/cert-manager-keyring-2021-09-20-1020CF3C033D4F35BAE1C19E1226061C665DF13E.gpg\n",
				},
			},
			expected: map[string]string{
				ocispec.AnnotationTitle:       "cert-manager",
				ocispec.AnnotationVersion:     "v1.19.3",
				ocispec.AnnotationDescription: "A Helm chart for cert-manager",
				ocispec.AnnotationURL:         "https://cert-manager.io",
				ocispec.AnnotationSource:      "https://github.com/cert-manager/cert-manager",
				ocispec.AnnotationAuthors:     "cert-manager-maintainers (cert-manager-maintainers@googlegroups.com)",
				ocispec.AnnotationCreated:     createdStr,
				"artifacthub.io/category":     "security",
				"artifacthub.io/license":      "Apache-2.0",
				"artifacthub.io/prerelease":   "false",
				"artifacthub.io/signKey":      "fingerprint: 1020CF3C033D4F35BAE1C19E1226061C665DF13E\nurl: https://cert-manager.io/public-keys/cert-manager-keyring-2021-09-20-1020CF3C033D4F35BAE1C19E1226061C665DF13E.gpg\n",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateOCIAnnotations(tt.meta, fixedTime)

			// Check every expected annotation is present and correct
			for key, want := range tt.expected {
				if val, ok := got[key]; !ok {
					t.Errorf("missing annotation %s\n  expected: %q", key, want)
				} else if val != want {
					t.Errorf("annotation %s\n  expected: %q\n       got: %q", key, want, val)
				}
			}

			// Check no unexpected annotations exist
			for key := range got {
				if _, ok := tt.expected[key]; !ok {
					t.Errorf("unexpected annotation %s = %q", key, got[key])
				}
			}

			// Check absent annotations
			for _, key := range tt.absent {
				assertAbsent(t, got, key)
			}

			// Log the full annotation set for manual inspection
			t.Logf("=== %s annotations ===", tt.name)
			for k, v := range got {
				t.Logf("  %s: %q", k, v)
			}
		})
	}
}

func assertEqual(t *testing.T, annotations map[string]string, key, expected string) {
	t.Helper()
	if got, ok := annotations[key]; !ok || got != expected {
		t.Errorf("annotation %s: expected %q, got %q (present=%v)", key, expected, got, ok)
	}
}

func assertAbsent(t *testing.T, annotations map[string]string, key string) {
	t.Helper()
	if _, ok := annotations[key]; ok {
		t.Errorf("annotation %s should be absent, but got %q", key, annotations[key])
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
