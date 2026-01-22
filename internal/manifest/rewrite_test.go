package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"sigs.k8s.io/yaml"
)

func TestShouldRewriteURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{"empty URL", "", false},
		{"https URL", "https://charts.bitnami.com/bitnami", true},
		{"http URL", "http://charts.example.com/charts", true},
		{"oci URL", "oci://registry.example.com/charts", true},
		{"file URL", "file://./charts/mychart", false},
		{"file URL no double slash", "file:./charts/mychart", false},
		{"alias with @", "@bitnami", false},
		{"alias with prefix", "alias:bitnami", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldRewriteURL(tt.url)
			if result != tt.expected {
				t.Errorf("shouldRewriteURL(%q) = %v, want %v", tt.url, result, tt.expected)
			}
		})
	}
}

func TestRewriteDependencyURL(t *testing.T) {
	proxyHost := "chartproxy.example.com"

	tests := []struct {
		name        string
		originalURL string
		proxyHost   string
		wantURL     string
		wantOCI     bool
		wantErr     bool
	}{
		{
			name:        "https URL with path",
			originalURL: "https://charts.bitnami.com/bitnami",
			proxyHost:   proxyHost,
			wantURL:     "oci://chartproxy.example.com/charts.bitnami.com/bitnami",
			wantOCI:     false,
			wantErr:     false,
		},
		{
			name:        "https URL without path",
			originalURL: "https://charts.jetstack.io",
			proxyHost:   proxyHost,
			wantURL:     "oci://chartproxy.example.com/charts.jetstack.io",
			wantOCI:     false,
			wantErr:     false,
		},
		{
			name:        "http URL",
			originalURL: "http://charts.example.com/repo",
			proxyHost:   proxyHost,
			wantURL:     "oci://chartproxy.example.com/charts.example.com/repo",
			wantOCI:     false,
			wantErr:     false,
		},
		{
			name:        "oci URL",
			originalURL: "oci://registry.example.com/charts/mychart",
			proxyHost:   proxyHost,
			wantURL:     "oci://chartproxy.example.com/registry.example.com/charts/mychart",
			wantOCI:     true,
			wantErr:     false,
		},
		{
			name:        "empty URL",
			originalURL: "",
			proxyHost:   proxyHost,
			wantErr:     true,
		},
		{
			name:        "unsupported scheme",
			originalURL: "ftp://charts.example.com",
			proxyHost:   proxyHost,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotOCI, err := rewriteDependencyURL(tt.originalURL, tt.proxyHost)
			if (err != nil) != tt.wantErr {
				t.Errorf("rewriteDependencyURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if gotURL != tt.wantURL {
					t.Errorf("rewriteDependencyURL() gotURL = %v, want %v", gotURL, tt.wantURL)
				}
				if gotOCI != tt.wantOCI {
					t.Errorf("rewriteDependencyURL() gotOCI = %v, want %v", gotOCI, tt.wantOCI)
				}
			}
		})
	}
}

func TestGetSkipReason(t *testing.T) {
	tests := []struct {
		url      string
		expected string
	}{
		{"", "empty URL"},
		{"@bitnami", "Helm repo alias"},
		{"alias:bitnami", "Helm repo alias"},
		{"file://./mychart", "local file reference"},
		{"file:./mychart", "local file reference"},
		{"something-else", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			result := getSkipReason(tt.url)
			if result != tt.expected {
				t.Errorf("getSkipReason(%q) = %v, want %v", tt.url, result, tt.expected)
			}
		})
	}
}

// createTestTarball creates a gzipped tarball with a Chart.yaml for testing
func createTestTarball(chartName string, metadata *chart.Metadata) ([]byte, error) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Marshal Chart.yaml
	chartYAML, err := yaml.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	// Add Chart.yaml
	chartYAMLPath := chartName + "/Chart.yaml"
	header := &tar.Header{
		Name: chartYAMLPath,
		Mode: 0644,
		Size: int64(len(chartYAML)),
	}
	if err := tw.WriteHeader(header); err != nil {
		return nil, err
	}
	if _, err := tw.Write(chartYAML); err != nil {
		return nil, err
	}

	// Add a dummy values.yaml
	valuesYAML := []byte("key: value\n")
	valuesPath := chartName + "/values.yaml"
	header = &tar.Header{
		Name: valuesPath,
		Mode: 0644,
		Size: int64(len(valuesYAML)),
	}
	if err := tw.WriteHeader(header); err != nil {
		return nil, err
	}
	if _, err := tw.Write(valuesYAML); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func TestExtractChartYAML(t *testing.T) {
	metadata := &chart.Metadata{
		APIVersion: "v2",
		Name:       "test-chart",
		Version:    "1.0.0",
		Dependencies: []*chart.Dependency{
			{
				Name:       "redis",
				Version:    "17.0.0",
				Repository: "https://charts.bitnami.com/bitnami",
			},
		},
	}

	tarball, err := createTestTarball("test-chart", metadata)
	if err != nil {
		t.Fatalf("failed to create test tarball: %v", err)
	}

	extracted, path, err := extractChartYAML(tarball)
	if err != nil {
		t.Fatalf("extractChartYAML() error = %v", err)
	}

	if path != "test-chart/Chart.yaml" {
		t.Errorf("extractChartYAML() path = %v, want test-chart/Chart.yaml", path)
	}

	if extracted.Name != metadata.Name {
		t.Errorf("extractChartYAML() name = %v, want %v", extracted.Name, metadata.Name)
	}

	if len(extracted.Dependencies) != 1 {
		t.Errorf("extractChartYAML() dependencies count = %v, want 1", len(extracted.Dependencies))
	}
}

func TestRewriteChartDependencies(t *testing.T) {
	proxyHost := "chartproxy.example.com"

	t.Run("chart with dependencies", func(t *testing.T) {
		metadata := &chart.Metadata{
			APIVersion: "v2",
			Name:       "test-chart",
			Version:    "1.0.0",
			Dependencies: []*chart.Dependency{
				{
					Name:       "redis",
					Version:    "17.0.0",
					Repository: "https://charts.bitnami.com/bitnami",
				},
				{
					Name:       "postgresql",
					Version:    "12.0.0",
					Repository: "https://charts.bitnami.com/bitnami",
				},
			},
		}

		tarball, err := createTestTarball("test-chart", metadata)
		if err != nil {
			t.Fatalf("failed to create test tarball: %v", err)
		}

		rewritten, result, err := rewriteChartDependencies(tarball, proxyHost, nil)
		if err != nil {
			t.Fatalf("rewriteChartDependencies() error = %v", err)
		}

		if !result.Modified {
			t.Error("rewriteChartDependencies() Modified = false, want true")
		}

		if len(result.Dependencies) != 2 {
			t.Errorf("rewriteChartDependencies() dependencies count = %v, want 2", len(result.Dependencies))
		}

		// Extract and verify the rewritten tarball
		newMetadata, _, err := extractChartYAML(rewritten)
		if err != nil {
			t.Fatalf("failed to extract rewritten tarball: %v", err)
		}

		for _, dep := range newMetadata.Dependencies {
			expectedURL := "oci://chartproxy.example.com/charts.bitnami.com/bitnami"
			if dep.Repository != expectedURL {
				t.Errorf("dependency %s repository = %v, want %v", dep.Name, dep.Repository, expectedURL)
			}
		}
	})

	t.Run("chart without dependencies", func(t *testing.T) {
		metadata := &chart.Metadata{
			APIVersion: "v2",
			Name:       "test-chart",
			Version:    "1.0.0",
		}

		tarball, err := createTestTarball("test-chart", metadata)
		if err != nil {
			t.Fatalf("failed to create test tarball: %v", err)
		}

		rewritten, result, err := rewriteChartDependencies(tarball, proxyHost, nil)
		if err != nil {
			t.Fatalf("rewriteChartDependencies() error = %v", err)
		}

		if result.Modified {
			t.Error("rewriteChartDependencies() Modified = true, want false")
		}

		// Should return original tarball
		if !bytes.Equal(rewritten, tarball) {
			t.Error("rewriteChartDependencies() should return original tarball when no dependencies")
		}
	})

	t.Run("chart with mixed dependencies", func(t *testing.T) {
		metadata := &chart.Metadata{
			APIVersion: "v2",
			Name:       "test-chart",
			Version:    "1.0.0",
			Dependencies: []*chart.Dependency{
				{
					Name:       "redis",
					Version:    "17.0.0",
					Repository: "https://charts.bitnami.com/bitnami",
				},
				{
					Name:       "local-chart",
					Version:    "1.0.0",
					Repository: "file://../local-chart",
				},
				{
					Name:       "aliased-chart",
					Version:    "1.0.0",
					Repository: "@myrepo",
				},
			},
		}

		tarball, err := createTestTarball("test-chart", metadata)
		if err != nil {
			t.Fatalf("failed to create test tarball: %v", err)
		}

		rewritten, result, err := rewriteChartDependencies(tarball, proxyHost, nil)
		if err != nil {
			t.Fatalf("rewriteChartDependencies() error = %v", err)
		}

		if !result.Modified {
			t.Error("rewriteChartDependencies() Modified = false, want true")
		}

		if len(result.Dependencies) != 3 {
			t.Errorf("result.Dependencies count = %v, want 3", len(result.Dependencies))
		}

		// Check that redis was rewritten
		found := false
		for _, dep := range result.Dependencies {
			if dep.Name == "redis" && !dep.Skipped {
				found = true
				if dep.NewURL != "oci://chartproxy.example.com/charts.bitnami.com/bitnami" {
					t.Errorf("redis NewURL = %v, want oci://chartproxy.example.com/charts.bitnami.com/bitnami", dep.NewURL)
				}
			}
		}
		if !found {
			t.Error("redis dependency not found in result")
		}

		// Verify in the tarball
		newMetadata, _, err := extractChartYAML(rewritten)
		if err != nil {
			t.Fatalf("failed to extract rewritten tarball: %v", err)
		}

		for _, dep := range newMetadata.Dependencies {
			switch dep.Name {
			case "redis":
				if dep.Repository != "oci://chartproxy.example.com/charts.bitnami.com/bitnami" {
					t.Errorf("redis repository = %v, want oci://...", dep.Repository)
				}
			case "local-chart":
				if dep.Repository != "file://../local-chart" {
					t.Errorf("local-chart repository = %v, want file://../local-chart", dep.Repository)
				}
			case "aliased-chart":
				if dep.Repository != "@myrepo" {
					t.Errorf("aliased-chart repository = %v, want @myrepo", dep.Repository)
				}
			}
		}
	})
}
