package manifest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
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
// are kept. The layer is served later by streaming it from the upstream again.
func (m *Manifests) prepareStreamed(ctx context.Context, path string, chartVer *repo.ChartVersion, downloadURL, reference string) *errors.RegError {
	body, err := m.openUpstream(ctx, downloadURL)
	if err != nil {
		return downloadError(err, "MANIFEST_UNKNOWN")
	}
	defer body.Close()

	hasher := sha256.New()
	counter := &countingReader{r: io.LimitReader(body, maxDownloadSize+1)}
	br := bufio.NewReader(io.TeeReader(counter, hasher))

	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return upstreamError("reading upstream %s: %v", downloadURL, err)
	}
	// Some hosts answer 200 with an HTML page for missing files; packaging that
	// would hand clients a chart that cannot be installed.
	if !isGzip(magic) {
		return upstreamError("upstream %s did not return a gzip chart archive", downloadURL)
	}
	chartMeta, err := readChartMetadata(br)
	if err != nil {
		if counter.n > maxDownloadSize {
			return upstreamError("upstream %s response exceeds %d bytes", downloadURL, maxDownloadSize)
		}
		return upstreamError("upstream %s did not return a valid chart archive: %v", downloadURL, err)
	}
	// The layer digest covers every byte, including what follows the tar end.
	if _, err := io.Copy(io.Discard, br); err != nil {
		return upstreamError("reading upstream %s: %v", downloadURL, err)
	}
	if counter.n > maxDownloadSize {
		return upstreamError("upstream %s response exceeds %d bytes", downloadURL, maxDownloadSize)
	}
	layerHex := hex.EncodeToString(hasher.Sum(nil))
	m.checkIndexDigest(chartVer, layerHex)

	layer := layerDescriptor(downloadURL, digest.NewDigestFromEncoded(digest.SHA256, layerHex), counter.n)
	return m.storeChart(ctx, path, chartVer, reference, chartMeta, layer, nil, downloadURL)
}
