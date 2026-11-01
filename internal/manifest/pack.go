package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func hashOf(d digest.Digest) (v1.Hash, error) {
	return v1.NewHash(d.String())
}

// storeChart packs the OCI manifest of a chart version and stores it with its
// config blob. The layer is stored when given; otherwise it is registered to be
// streamed from sourceURL. Both preparation paths go through here, so a chart
// gets the same manifest digest whichever path prepared it.
func (m *Manifests) storeChart(ctx context.Context, path string, chartVer *repo.ChartVersion, reference string, meta *chart.Metadata, layerDesc ocispec.Descriptor, layer []byte, sourceURL string) *errors.RegError {
	configData, err := json.Marshal(meta)
	if err != nil {
		return errors.RegErrInternal(err)
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
	// The config descriptor inside the manifest carries no annotations.
	configDesc.Annotations = nil
	packOpts := oras.PackOptions{
		ManifestAnnotations: generateOCIAnnotations(meta, getDeterministicCreatedTimestamp(chartVer)),
		ConfigDescriptor:    &configDesc,
		PackImageManifest:   true,
	}
	// Pack only marshals descriptors; the layer content is never read.
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
	layerHash, err := hashOf(layerDesc.Digest)
	if err != nil {
		return errors.RegErrInternal(err)
	}

	// Cleanup must not delete these blobs between their Put and the manifest
	// Write that references them. The writes are in memory and are not stopped
	// halfway, which would leave blobs that no manifest references.
	storeCtx := context.WithoutCancel(ctx)
	m.copyMu.RLock()
	defer m.copyMu.RUnlock()
	if err := put.Put(storeCtx, "", configHash, io.NopCloser(bytes.NewReader(configData))); err != nil {
		return errors.RegErrInternal(err)
	}
	if layer != nil {
		if err := put.Put(storeCtx, "", layerHash, io.NopCloser(bytes.NewReader(layer))); err != nil {
			return errors.RegErrInternal(err)
		}
	} else {
		m.blobs.addSource(layerDesc.Digest, sourceURL, layerDesc.Size)
	}

	repoKey := fmt.Sprintf("%s/%s", path, chartVer.Name)
	ma := Manifest{
		ContentType: root.MediaType,
		Blob:        manifestJSON,
		Refs:        []string{configHash.String(), layerHash.String()},
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
