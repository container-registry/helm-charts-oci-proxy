package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	stderrors "errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"sigs.k8s.io/yaml"
)

// Logger interface for logging
type Logger interface {
	Printf(format string, v ...interface{})
}

// rewriteReader yields a chart archive with its dependencies rewritten while
// the archive it reads from streams in.
type rewriteReader struct {
	*io.PipeReader
	src  io.Closer
	done chan struct{}
	// modified is valid once Read returned io.EOF.
	modified bool
	err      error // valid after Close
}

// rewriteError is a failure of the rewrite itself on an archive that was read
// fine, such as an entry archive/tar reads but cannot write.
type rewriteError struct{ error }

func (e rewriteError) Unwrap() error { return e.error }

// failed reports whether the rewrite itself failed. Call it after Close.
func (r *rewriteReader) failed() bool {
	var re rewriteError
	return stderrors.As(r.err, &re)
}

// Close stops the rewrite and returns once it no longer reads src, so a
// caller may inspect what src counted.
func (r *rewriteReader) Close() error {
	r.PipeReader.Close()
	err := r.src.Close()
	<-r.done
	return err
}

// rewriteStream rewrites the chart archive src as it is read. src is read to
// its end, so a hash taken while it streams covers every byte.
func rewriteStream(src io.ReadCloser, proxyHost string, log Logger) *rewriteReader {
	pr, pw := io.Pipe()
	r := &rewriteReader{PipeReader: pr, src: src, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		modified, err := rewriteChart(pw, src, proxyHost, log)
		if err == nil {
			_, err = io.Copy(io.Discard, src)
		}
		r.modified, r.err = modified, err
		pw.CloseWithError(err)
	}()
	return r
}

// rewriteChart copies the chart archive src to dst with the dependency
// repositories of the chart's own Chart.yaml pointed at proxyHost; a
// subchart's charts/<x>/Chart.yaml is copied as it is. The copy is always
// recompressed, which gives the same bytes for the same input; modified
// reports whether any dependency was rewritten.
func rewriteChart(dst io.Writer, src io.Reader, proxyHost string, log Logger) (modified bool, err error) {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return false, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	zw := gzip.NewWriter(dst)
	tw := tar.NewWriter(zw)
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
		var data io.Reader = tr
		if _, name, _ := strings.Cut(hd.Name, "/"); name == "Chart.yaml" {
			if hd.Size > loader.MaxDecompressedFileSize {
				return false, fmt.Errorf("Chart.yaml is larger than the maximum file size %d", loader.MaxDecompressedFileSize)
			}
			chartYAML, err := io.ReadAll(tr)
			if err != nil {
				return false, err
			}
			if out, ok := rewriteChartYAML(chartYAML, proxyHost, log); ok {
				chartYAML, modified = out, true
				h := *hd
				h.Size = int64(len(out))
				hd = &h
			}
			data = bytes.NewReader(chartYAML)
		}
		if err := tw.WriteHeader(hd); err != nil {
			return false, rewriteError{err}
		}
		if _, err := io.Copy(tw, data); err != nil {
			return false, err
		}
	}
	if err := tw.Close(); err != nil {
		return false, err
	}
	return modified, zw.Close()
}

// rewriteChartYAML points the dependency repositories of a Chart.yaml at
// proxyHost and reports false when there is nothing to rewrite. A Chart.yaml
// that does not parse is left to readChartMetadata, which rejects the chart.
func rewriteChartYAML(data []byte, proxyHost string, log Logger) ([]byte, bool) {
	var md chart.Metadata
	if err := yaml.Unmarshal(data, &md); err != nil {
		return nil, false
	}
	modified := false
	for _, dep := range md.Dependencies {
		if dep == nil || !shouldRewriteURL(dep.Repository) {
			continue
		}
		newURL, wasOCI, err := rewriteDependencyURL(dep.Repository, proxyHost)
		if err != nil {
			log.Printf("warning: failed to rewrite dependency %s URL %s: %v", dep.Name, dep.Repository, err)
			continue
		}
		if wasOCI {
			log.Printf("debug: rewriting OCI dependency %s URL %s (proxy may not support OCI-to-OCI proxying yet)", dep.Name, dep.Repository)
		}
		dep.Repository = newURL
		modified = true
	}
	if !modified {
		return nil, false
	}
	out, err := yaml.Marshal(&md)
	return out, err == nil
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
