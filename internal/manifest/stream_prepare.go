package manifest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"path/filepath"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
)

// hashingReader hashes and counts what is read through it.
type hashingReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func newHashingReader(r io.Reader) *hashingReader {
	return &hashingReader{r: r, h: sha256.New()}
}

func (c *hashingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	return n, err
}

func (c *hashingReader) digest() digest.Digest {
	return digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(c.h.Sum(nil)))
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
		return upstreamError("upstream %s did not return a valid chart archive: %v", downloadURL, err)
	}
	// The layer digest covers every byte, including what follows the tar end.
	if _, err := io.Copy(io.Discard, archive); err != nil {
		return upstreamError("reading upstream %s: %v", downloadURL, err)
	}
	if counter.n > maxDownloadSize {
		return upstreamError("upstream %s response exceeds %d bytes", downloadURL, maxDownloadSize)
	}
	m.checkIndexDigest(chartVer, counter.digest().Encoded())
	// A chart without dependencies to rewrite keeps the upstream's bytes.
	if rw != nil && !rw.modified {
		served, rewriteHost = counter, ""
	}
	layer := layerDescriptor(downloadURL, served.digest(), served.n)
	return m.storeChart(repoKey, chartVer, reference, chartMeta, layer, rewriteHost, downloadURL)
}
