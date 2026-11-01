package manifest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
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

func hashOf(d digest.Digest) (v1.Hash, error) {
	return v1.NewHash(d.String())
}

func upstreamError(format string, a ...any) *errors.RegError {
	return &errors.RegError{Status: http.StatusBadGateway, Code: "UPSTREAM_ERROR", Message: fmt.Sprintf(format, a...)}
}

// prepareStreamed builds the OCI manifest for a chart version from a single
// streamed read of its tarball: the bytes are hashed and counted as they pass,
// Chart.yaml is extracted on the way, and nothing but the config blob and the
// manifest is kept. The layer is served later by streaming it from the
// upstream again. The manifest is byte-identical to the one built from a fully
// buffered tarball.
func (m *Manifests) prepareStreamed(ctx context.Context, path string, chartVer *repo.ChartVersion, downloadURL, reference string) *errors.RegError {
	body, err := m.openUpstream(ctx, downloadURL)
	if err != nil {
		return downloadError(err, "MANIFEST_UNKNOWN")
	}
	defer body.Close()

	hasher := sha256.New()
	counter := &countingReader{r: io.LimitReader(body, maxDownloadSize+1)}
	br := bufio.NewReader(io.TeeReader(counter, hasher))

	// Some hosts answer 200 with an HTML page for missing files; packaging that
	// would hand clients a chart that cannot be installed.
	if magic, err := br.Peek(2); err != nil || !isGzip(magic) {
		return upstreamError("upstream %s did not return a gzip chart archive", downloadURL)
	}
	chartMeta, err := readChartMetadata(br)
	if err != nil {
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

	configData, err := json.Marshal(chartMeta)
	if err != nil {
		return errors.RegErrInternal(err)
	}
	packOpts := oras.PackOptions{
		ManifestAnnotations: generateOCIAnnotations(chartMeta, getDeterministicCreatedTimestamp(chartVer)),
	}
	configDesc := ocispec.Descriptor{
		MediaType:   helmregistry.ConfigMediaType,
		Digest:      digest.FromBytes(configData),
		Size:        int64(len(configData)),
		Annotations: map[string]string{ocispec.AnnotationTitle: "$config"},
	}
	memStore := memory.New()
	if err := memStore.Push(ctx, configDesc, bytes.NewReader(configData)); err != nil {
		return errors.RegErrInternal(err)
	}
	configDesc.Annotations = packOpts.ConfigAnnotations
	packOpts.ConfigDescriptor = &configDesc
	packOpts.PackImageManifest = true

	layerDesc := ocispec.Descriptor{
		MediaType:   helmregistry.ChartLayerMediaType,
		Digest:      digest.NewDigestFromEncoded(digest.SHA256, layerHex),
		Size:        counter.n,
		Annotations: map[string]string{ocispec.AnnotationTitle: filepath.Clean(filepath.Base(downloadURL))},
	}
	// Pack only marshals descriptors; the layer content is never needed here.
	root, err := oras.Pack(ctx, memStore, "", []ocispec.Descriptor{layerDesc}, packOpts)
	if err != nil {
		return errors.RegErrInternal(err)
	}
	manifestJSON, err := content.FetchAll(ctx, memStore, root)
	if err != nil {
		return errors.RegErrInternal(err)
	}

	put, ok := m.blobHandler.(handler.BlobPutHandler)
	if !ok {
		return errors.RegErrInternal(fmt.Errorf("blob handler does not accept writes"))
	}
	configHash, err := hashOf(configDesc.Digest)
	if err != nil {
		return errors.RegErrInternal(err)
	}
	// Same exclusion as the buffered copy: cleanup must not delete the config
	// blob between this Put and the manifest Write that references it.
	m.copyMu.RLock()
	defer m.copyMu.RUnlock()
	if err := put.Put(ctx, "", configHash, io.NopCloser(bytes.NewReader(configData))); err != nil {
		return errors.RegErrInternal(err)
	}
	m.blobs.addSource(layerDesc.Digest, downloadURL, layerDesc.Size)

	repoKey := fmt.Sprintf("%s/%s", path, chartVer.Name)
	ma := Manifest{
		ContentType: root.MediaType,
		Blob:        manifestJSON,
		Refs:        []string{configDesc.Digest.String(), layerDesc.Digest.String()},
		CreatedAt:   time.Now(),
	}
	if err := m.Write(repoKey, root.Digest.String(), ma); err != nil {
		return errors.RegErrInternal(err)
	}
	if reference != "" {
		if err := m.Write(repoKey, reference, ma); err != nil {
			return errors.RegErrInternal(err)
		}
	}
	chartsPrepared.Add(1)
	return nil
}
