package manifest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
)

type hashingReader struct {
	r io.Reader
	d digest.Digester
	n int64
}

func newHashingReader(r io.Reader) *hashingReader {
	d := digest.SHA256.Digester()
	return &hashingReader{r: io.TeeReader(r, d.Hash()), d: d}
}

func (c *hashingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func upstreamError(format string, a ...any) *errors.RegError {
	return &errors.RegError{Status: http.StatusBadGateway, Code: "UPSTREAM_ERROR", Message: fmt.Sprintf(format, a...)}
}

func layerDescriptor(downloadURL string, d digest.Digest, size int64) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType:   helmregistry.ChartLayerMediaType,
		Digest:      d,
		Size:        size,
		Annotations: map[string]string{ocispec.AnnotationTitle: filepath.Clean(filepath.Base(downloadURL))},
	}
}

// prepareStreamed builds the OCI manifest for a chart version from a single
// streamed read of its tarball: the bytes are hashed and counted as they pass,
// the chart is validated on the way, and only the config blob and the manifest
// are kept. With rewriteHost set, the dependencies are rewritten in the same
// pass. The layer is served later by streaming it from the upstream again.
func (m *Manifests) prepareStreamed(ctx context.Context, repoKey string, chartVer *repo.ChartVersion, downloadURL, reference, rewriteHost string) *errors.RegError {
	body, err := m.openUpstream(ctx, downloadURL)
	if err != nil {
		return downloadError(err, "MANIFEST_UNKNOWN")
	}
	defer body.Close()

	counter := newHashingReader(io.LimitReader(body, maxDownloadSize+1))
	br := bufio.NewReader(counter)

	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return upstreamError("reading upstream %s: %v", downloadURL, err)
	}
	// Some hosts answer 200 with an HTML page for missing files; packaging that
	// would hand clients a chart that cannot be installed.
	if !isGzip(magic) {
		return upstreamError("upstream %s did not return a gzip chart archive", downloadURL)
	}
	// served is the archive clients get: the upstream's or its rewrite.
	served := counter
	var archive io.Reader = br
	var rw *rewriteReader
	if rewriteHost != "" {
		rw = rewriteStream(struct {
			io.Reader
			io.Closer
		}{br, body}, rewriteHost, m.log)
		defer rw.Close()
		served = newHashingReader(rw)
		archive = served
	}
	chartMeta, err := readChartMetadata(archive)
	if err != nil {
		if rw != nil {
			rw.Close()
		}
		if counter.n > maxDownloadSize {
			return upstreamError("upstream %s response exceeds %d bytes", downloadURL, maxDownloadSize)
		}
		// A chart whose dependencies cannot be rewritten is served as published.
		if rw != nil && rw.failed() {
			m.log.Printf("warning: failed to rewrite dependencies of %s, serving the original: %v", downloadURL, err)
			return m.prepareStreamed(ctx, repoKey, chartVer, downloadURL, reference, "")
		}
		return upstreamError("upstream %s did not return a valid chart archive: %v", downloadURL, err)
	}
	// The layer digest covers every byte, including what follows the tar end.
	if _, err := io.Copy(io.Discard, archive); err != nil {
		return upstreamError("reading upstream %s: %v", downloadURL, err)
	}
	if counter.n > maxDownloadSize {
		return upstreamError("upstream %s response exceeds %d bytes", downloadURL, maxDownloadSize)
	}
	m.checkIndexDigest(chartVer, counter.d.Digest().Encoded())
	// A chart without dependencies to rewrite keeps the upstream's bytes.
	if rw != nil && !rw.modified {
		served, rewriteHost = counter, ""
	}
	layer := layerDescriptor(downloadURL, served.d.Digest(), served.n)
	return m.storeChart(repoKey, chartVer, reference, chartMeta, layer, rewriteHost, downloadURL)
}
