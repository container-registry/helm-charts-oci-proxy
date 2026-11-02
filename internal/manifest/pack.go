package manifest

import (
	"encoding/json"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
)

// storeChart builds the OCI manifest of a chart version and stores its record.
// The layer is streamed from sourceURL, with its dependencies rewritten when
// rewriteHost is set.
func (m *Manifests) storeChart(repoKey string, chartVer *repo.ChartVersion, reference string, meta *chart.Metadata, layerDesc ocispec.Descriptor, rewriteHost, sourceURL string) *errors.RegError {
	configData, err := json.Marshal(meta)
	if err != nil {
		return errors.RegErrInternal(err)
	}
	configDesc := ocispec.Descriptor{
		MediaType: helmregistry.ConfigMediaType,
		Digest:    digest.FromBytes(configData),
		Size:      int64(len(configData)),
	}
	// The same fields, in the same order, that oras.Pack wrote before; the
	// created annotation is always set, so nothing else is added.
	manifestJSON, err := json.Marshal(ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      configDesc,
		Layers:      []ocispec.Descriptor{layerDesc},
		Annotations: generateOCIAnnotations(meta, getDeterministicCreatedTimestamp(chartVer)),
	})
	if err != nil {
		return errors.RegErrInternal(err)
	}

	layer := &blob{digest: layerDesc.Digest, size: layerDesc.Size, urls: []string{sourceURL}, rewriteHost: rewriteHost}
	d := digest.FromBytes(manifestJSON).String()
	refs := []string{d}
	if reference != "" {
		refs = append(refs, reference)
	}
	m.insert(repoKey, &record{
		manifest: manifestJSON,
		digest:   d,
		config:   &blob{digest: configDesc.Digest, size: configDesc.Size, data: configData},
		layer:    layer,
		expires:  time.Now().Add(m.config.CacheTTL),
	}, refs...)
	chartsPrepared.Add(1)
	return nil
}
