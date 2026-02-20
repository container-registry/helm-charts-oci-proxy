//go:build integration

package manifest

import (
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/repo"
	"sigs.k8s.io/yaml"
)

// TestAnnotationsPipeline_Integration downloads real chart tarballs from public
// chart repositories and verifies the full extractChartMeta → generateOCIAnnotations
// pipeline produces correct OCI annotations.
//
// Run with: go test ./internal/manifest/ -tags integration -run TestAnnotationsPipeline
func TestAnnotationsPipeline_Integration(t *testing.T) {
	fixedTime := time.Date(2025, 12, 15, 17, 31, 44, 0, time.UTC)

	tests := []struct {
		name     string
		indexURL string // chart repo index.yaml URL
		chart    string // chart name in the index
		version  string // chart version to fetch
		// Expected annotations (key → value). Empty string means "must be present and non-empty".
		expected map[string]string
		// Keys that must be absent
		absent []string
	}{
		{
			name:     "harbor",
			indexURL: "https://helm.goharbor.io/index.yaml",
			chart:    "harbor",
			version:  "1.18.2",
			expected: map[string]string{
				ocispec.AnnotationTitle:       "harbor",
				ocispec.AnnotationVersion:     "1.18.2",
				ocispec.AnnotationDescription: "An open source trusted cloud native registry that stores, signs, and scans content",
				ocispec.AnnotationURL:         "https://goharbor.io",
				ocispec.AnnotationSource:      "https://github.com/goharbor/harbor",
				ocispec.AnnotationAuthors:     "", // non-empty, but exact value may change with maintainer list
				ocispec.AnnotationCreated:     fixedTime.Format(time.RFC3339),
			},
		},
		{
			name:     "ingress-nginx",
			indexURL: "https://kubernetes.github.io/ingress-nginx/index.yaml",
			chart:    "ingress-nginx",
			version:  "4.14.3",
			expected: map[string]string{
				ocispec.AnnotationTitle:       "ingress-nginx",
				ocispec.AnnotationVersion:     "4.14.3",
				ocispec.AnnotationDescription: "Ingress controller for Kubernetes using NGINX as a reverse proxy and load balancer",
				ocispec.AnnotationURL:         "https://github.com/kubernetes/ingress-nginx",
				ocispec.AnnotationSource:      "https://github.com/kubernetes/ingress-nginx",
				ocispec.AnnotationAuthors:     "", // non-empty
				ocispec.AnnotationCreated:     fixedTime.Format(time.RFC3339),
			},
		},
		{
			name:     "cert-manager",
			indexURL: "https://charts.jetstack.io/index.yaml",
			chart:    "cert-manager",
			version:  "v1.17.2",
			expected: map[string]string{
				ocispec.AnnotationTitle:       "cert-manager",
				ocispec.AnnotationVersion:     "v1.17.2",
				ocispec.AnnotationDescription: "A Helm chart for cert-manager",
				ocispec.AnnotationURL:         "https://cert-manager.io",
				ocispec.AnnotationSource:      "https://github.com/cert-manager/cert-manager",
				ocispec.AnnotationAuthors:     "", // non-empty
				ocispec.AnnotationCreated:     fixedTime.Format(time.RFC3339),
				// cert-manager has custom annotations in Chart.yaml
				"artifacthub.io/category": "security",
				"artifacthub.io/license":  "Apache-2.0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Step 1: Download index.yaml and find chart download URL
			chartURL := resolveChartURL(t, tt.indexURL, tt.chart, tt.version)
			t.Logf("chart URL: %s", chartURL)

			// Step 2: Download the chart tarball
			chartData := downloadBytes(t, chartURL)
			t.Logf("downloaded %d bytes", len(chartData))

			// Step 3: Extract metadata (the function under test)
			meta, err := extractChartMeta(chartData)
			if err != nil {
				t.Fatalf("extractChartMeta failed: %v", err)
			}
			t.Logf("extracted metadata: name=%s version=%s", meta.Name, meta.Version)

			// Step 4: Generate annotations (the function under test)
			annotations := generateOCIAnnotations(meta, fixedTime)

			// Step 5: Verify annotations
			for key, want := range tt.expected {
				got, ok := annotations[key]
				if !ok {
					t.Errorf("missing annotation %s", key)
					continue
				}
				if want == "" {
					// Just check presence and non-empty
					if got == "" {
						t.Errorf("annotation %s is empty, expected non-empty value", key)
					}
				} else {
					if got != want {
						t.Errorf("annotation %s\n  expected: %q\n       got: %q", key, want, got)
					}
				}
			}

			for _, key := range tt.absent {
				if val, ok := annotations[key]; ok {
					t.Errorf("annotation %s should be absent, got %q", key, val)
				}
			}

			// Log all annotations for manual inspection
			t.Logf("=== %s: %d annotations ===", tt.name, len(annotations))
			for k, v := range annotations {
				t.Logf("  %s: %q", k, v)
			}
		})
	}
}

// resolveChartURL fetches the repo index.yaml, finds the chart version, and returns the download URL.
func resolveChartURL(t *testing.T, indexURL, chartName, version string) string {
	t.Helper()

	data := downloadBytes(t, indexURL)

	idx := repo.NewIndexFile()
	if err := yaml.UnmarshalStrict(data, idx); err != nil {
		// Some indexes have extra fields; fall back to non-strict
		idx = repo.NewIndexFile()
		if err := yaml.Unmarshal(data, idx); err != nil {
			t.Fatalf("failed to parse index.yaml from %s: %v", indexURL, err)
		}
	}
	idx.SortEntries()

	cv, err := idx.Get(chartName, version)
	if err != nil {
		t.Fatalf("chart %s@%s not found in index: %v", chartName, version, err)
	}
	if len(cv.URLs) == 0 {
		t.Fatalf("chart %s@%s has no URLs", chartName, version)
	}

	chartURL := cv.URLs[0]
	// Resolve relative URLs against the index URL base
	parsed, err := neturl.Parse(chartURL)
	if err != nil {
		t.Fatalf("invalid chart URL %q: %v", chartURL, err)
	}
	if !parsed.IsAbs() {
		base, err := neturl.Parse(indexURL)
		if err != nil {
			t.Fatalf("invalid index URL %q: %v", indexURL, err)
		}
		chartURL = base.ResolveReference(parsed).String()
	}
	return chartURL
}

// downloadBytes fetches a URL and returns the response body.
func downloadBytes(t *testing.T, url string) []byte {
	t.Helper()

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("failed to download %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d for %s", resp.StatusCode, url)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response from %s: %v", url, err)
	}

	if len(data) == 0 {
		t.Fatalf("empty response from %s", url)
	}

	return data
}

// TestExtractChartMeta_Integration verifies extractChartMeta returns valid
// metadata from real chart tarballs, independent of annotation generation.
func TestExtractChartMeta_Integration(t *testing.T) {
	tests := []struct {
		name        string
		indexURL    string
		chart       string
		version     string
		wantName    string
		wantVersion string
		// Structural checks: these fields must be non-empty
		wantNonEmpty []string
	}{
		{
			name:         "harbor has description, home, sources, maintainers",
			indexURL:     "https://helm.goharbor.io/index.yaml",
			chart:        "harbor",
			version:      "1.18.2",
			wantName:     "harbor",
			wantVersion:  "1.18.2",
			wantNonEmpty: []string{"description", "home", "sources", "maintainers"},
		},
		{
			name:         "ingress-nginx has custom annotations",
			indexURL:     "https://kubernetes.github.io/ingress-nginx/index.yaml",
			chart:        "ingress-nginx",
			version:      "4.14.3",
			wantName:     "ingress-nginx",
			wantVersion:  "4.14.3",
			wantNonEmpty: []string{"description", "home", "sources", "maintainers", "annotations"},
		},
		{
			name:         "cert-manager has all metadata fields",
			indexURL:     "https://charts.jetstack.io/index.yaml",
			chart:        "cert-manager",
			version:      "v1.17.2",
			wantName:     "cert-manager",
			wantVersion:  "v1.17.2",
			wantNonEmpty: []string{"description", "home", "sources", "maintainers", "annotations"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chartURL := resolveChartURL(t, tt.indexURL, tt.chart, tt.version)
			chartData := downloadBytes(t, chartURL)

			meta, err := extractChartMeta(chartData)
			if err != nil {
				t.Fatalf("extractChartMeta: %v", err)
			}

			if meta.Name != tt.wantName {
				t.Errorf("Name: expected %q, got %q", tt.wantName, meta.Name)
			}
			if meta.Version != tt.wantVersion {
				t.Errorf("Version: expected %q, got %q", tt.wantVersion, meta.Version)
			}

			for _, field := range tt.wantNonEmpty {
				switch field {
				case "description":
					if meta.Description == "" {
						t.Errorf("Description is empty")
					}
				case "home":
					if meta.Home == "" {
						t.Errorf("Home is empty")
					}
				case "sources":
					if len(meta.Sources) == 0 {
						t.Errorf("Sources is empty")
					}
				case "maintainers":
					if len(meta.Maintainers) == 0 {
						t.Errorf("Maintainers is empty")
					}
					for i, m := range meta.Maintainers {
						if m.Name == "" {
							t.Errorf("Maintainer[%d].Name is empty", i)
						}
					}
				case "annotations":
					if len(meta.Annotations) == 0 {
						t.Errorf("Annotations map is empty")
					}
				default:
					t.Errorf("unknown field check: %s", field)
				}
			}

			t.Logf("metadata: name=%s version=%s description=%q home=%s sources=%v maintainers=%d annotations=%d",
				meta.Name, meta.Version, meta.Description, meta.Home,
				meta.Sources, len(meta.Maintainers), len(meta.Annotations))

			// Verify the full pipeline produces sensible output
			annotations := generateOCIAnnotations(meta, time.Now())
			requiredKeys := []string{
				ocispec.AnnotationTitle,
				ocispec.AnnotationVersion,
				ocispec.AnnotationCreated,
			}
			for _, key := range requiredKeys {
				if _, ok := annotations[key]; !ok {
					t.Errorf("pipeline output missing required annotation: %s", key)
				}
			}

			// Title and version must match chart metadata
			if annotations[ocispec.AnnotationTitle] != meta.Name {
				t.Errorf("title annotation %q != meta.Name %q", annotations[ocispec.AnnotationTitle], meta.Name)
			}
			if annotations[ocispec.AnnotationVersion] != meta.Version {
				t.Errorf("version annotation %q != meta.Version %q", annotations[ocispec.AnnotationVersion], meta.Version)
			}

			fmt.Printf("  [OK] %s: %d annotations generated from real chart tarball\n", tt.name, len(annotations))
		})
	}
}
