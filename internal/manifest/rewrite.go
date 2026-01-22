package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/url"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"sigs.k8s.io/yaml"
)

// RewriteOptions holds options for dependency URL rewriting passed from HTTP handler
type RewriteOptions struct {
	Enabled   bool   // Final decision: query param overrides env var
	ProxyHost string // From req.Host (or PROXY_HOST override)
}

// RewriteResult contains information about what was rewritten
type RewriteResult struct {
	Modified     bool
	Dependencies []RewrittenDependency
}

// RewrittenDependency records a single dependency URL rewrite
type RewrittenDependency struct {
	Name        string
	OriginalURL string
	NewURL      string
	Skipped     bool
	SkipReason  string
}

// Logger interface for logging
type Logger interface {
	Printf(format string, v ...interface{})
}

// rewriteChartDependencies extracts Chart.yaml from a tarball, rewrites dependency
// repository URLs to point through the proxy, and repacks the tarball.
// If no dependencies need rewriting, returns the original data unchanged.
func rewriteChartDependencies(tarballData []byte, proxyHost string, log Logger) ([]byte, *RewriteResult, error) {
	result := &RewriteResult{}

	// Extract Chart.yaml
	metadata, chartYAMLPath, err := extractChartYAML(tarballData)
	if err != nil {
		return tarballData, result, fmt.Errorf("failed to extract Chart.yaml: %w", err)
	}

	if metadata == nil || len(metadata.Dependencies) == 0 {
		// No dependencies to rewrite
		return tarballData, result, nil
	}

	// Rewrite dependency URLs
	modified := false
	for _, dep := range metadata.Dependencies {
		if dep == nil {
			continue
		}
		originalURL := dep.Repository

		if !shouldRewriteURL(originalURL) {
			result.Dependencies = append(result.Dependencies, RewrittenDependency{
				Name:        dep.Name,
				OriginalURL: originalURL,
				Skipped:     true,
				SkipReason:  getSkipReason(originalURL),
			})
			continue
		}

		newURL, wasOCI, err := rewriteDependencyURL(originalURL, proxyHost)
		if err != nil {
			if log != nil {
				log.Printf("warning: failed to rewrite dependency %s URL %s: %v", dep.Name, originalURL, err)
			}
			result.Dependencies = append(result.Dependencies, RewrittenDependency{
				Name:        dep.Name,
				OriginalURL: originalURL,
				Skipped:     true,
				SkipReason:  err.Error(),
			})
			continue
		}

		if wasOCI && log != nil {
			log.Printf("debug: rewriting OCI dependency %s URL %s (proxy may not support OCI-to-OCI proxying yet)", dep.Name, originalURL)
		}

		dep.Repository = newURL
		modified = true
		result.Dependencies = append(result.Dependencies, RewrittenDependency{
			Name:        dep.Name,
			OriginalURL: originalURL,
			NewURL:      newURL,
		})
	}

	if !modified {
		return tarballData, result, nil
	}

	result.Modified = true

	// Replace Chart.yaml in tarball
	newTarball, err := replaceChartYAML(tarballData, chartYAMLPath, metadata)
	if err != nil {
		return tarballData, result, fmt.Errorf("failed to replace Chart.yaml: %w", err)
	}

	return newTarball, result, nil
}

// extractChartYAML extracts Chart.yaml from a gzipped tarball and returns
// the parsed metadata and the path to Chart.yaml within the archive.
func extractChartYAML(tarballData []byte) (*chart.Metadata, string, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(tarballData))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Chart.yaml is typically at <chart-name>/Chart.yaml
		if strings.HasSuffix(header.Name, "/Chart.yaml") || header.Name == "Chart.yaml" {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, "", fmt.Errorf("failed to read Chart.yaml: %w", err)
			}

			var metadata chart.Metadata
			if err := yaml.Unmarshal(data, &metadata); err != nil {
				return nil, "", fmt.Errorf("failed to parse Chart.yaml: %w", err)
			}

			return &metadata, header.Name, nil
		}
	}

	return nil, "", fmt.Errorf("chart.yaml not found in tarball")
}

// replaceChartYAML replaces Chart.yaml in a gzipped tarball with new metadata
func replaceChartYAML(tarballData []byte, chartYAMLPath string, metadata *chart.Metadata) ([]byte, error) {
	// Marshal new metadata
	newChartYAML, err := yaml.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Chart.yaml: %w", err)
	}

	// Read original tarball
	gzr, err := gzip.NewReader(bytes.NewReader(tarballData))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)

	// Create new tarball
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		if header.Name == chartYAMLPath {
			// Replace Chart.yaml
			newHeader := *header
			newHeader.Size = int64(len(newChartYAML))

			if err := tw.WriteHeader(&newHeader); err != nil {
				return nil, fmt.Errorf("failed to write tar header: %w", err)
			}
			if _, err := tw.Write(newChartYAML); err != nil {
				return nil, fmt.Errorf("failed to write Chart.yaml: %w", err)
			}
		} else {
			// Copy entry as-is
			if err := tw.WriteHeader(header); err != nil {
				return nil, fmt.Errorf("failed to write tar header: %w", err)
			}
			if header.Size > 0 {
				if _, err := io.Copy(tw, tr); err != nil {
					return nil, fmt.Errorf("failed to copy tar entry: %w", err)
				}
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}

	return buf.Bytes(), nil
}

// rewriteDependencyURL transforms a dependency repository URL to go through the proxy.
// Returns (newURL, wasOCI, error) where wasOCI indicates if the original was an OCI URL.
//
// Transformation: https://<host>/<path> -> oci://<proxy-host>/<host>/<path>
func rewriteDependencyURL(originalURL string, proxyHost string) (string, bool, error) {
	if originalURL == "" {
		return "", false, fmt.Errorf("empty URL")
	}

	parsed, err := url.Parse(originalURL)
	if err != nil {
		return "", false, fmt.Errorf("failed to parse URL: %w", err)
	}

	wasOCI := false
	var host, path string

	switch parsed.Scheme {
	case "https", "http":
		host = parsed.Host
		path = strings.TrimPrefix(parsed.Path, "/")
	case "oci":
		wasOCI = true
		host = parsed.Host
		path = strings.TrimPrefix(parsed.Path, "/")
	default:
		return "", false, fmt.Errorf("unsupported scheme: %s", parsed.Scheme)
	}

	// Build new URL: oci://<proxy-host>/<original-host>/<path>
	var newURL string
	if path != "" {
		newURL = fmt.Sprintf("oci://%s/%s/%s", proxyHost, host, path)
	} else {
		newURL = fmt.Sprintf("oci://%s/%s", proxyHost, host)
	}

	return newURL, wasOCI, nil
}

// shouldRewriteURL returns true if the repository URL should be rewritten.
// URLs that should NOT be rewritten:
// - Empty URLs
// - file:// URLs (local dependencies)
// - @alias or alias: references (Helm repo aliases)
func shouldRewriteURL(repoURL string) bool {
	if repoURL == "" {
		return false
	}

	// Check for Helm repo alias references
	if strings.HasPrefix(repoURL, "@") || strings.HasPrefix(repoURL, "alias:") {
		return false
	}

	// Check for file:// URLs
	if strings.HasPrefix(repoURL, "file://") || strings.HasPrefix(repoURL, "file:") {
		return false
	}

	return true
}

// getSkipReason returns a human-readable reason why a URL was skipped
func getSkipReason(repoURL string) string {
	if repoURL == "" {
		return "empty URL"
	}
	if strings.HasPrefix(repoURL, "@") || strings.HasPrefix(repoURL, "alias:") {
		return "Helm repo alias"
	}
	if strings.HasPrefix(repoURL, "file://") || strings.HasPrefix(repoURL, "file:") {
		return "local file reference"
	}
	return "unknown"
}
