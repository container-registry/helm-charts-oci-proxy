package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/container-registry/helm-charts-oci-proxy/internal/helper"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"io"
	"net/http"
	"net/url"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"strings"
	"time"
)

// extractChartMeta extracts metadata from a Helm chart archive.
// This is used to populate the OCI config layer with Chart.yaml contents.
func extractChartMeta(chartData []byte) (*chart.Metadata, error) {
	ch, err := loader.LoadArchive(bytes.NewReader(chartData))
	if err != nil {
		return nil, err
	}
	return ch.Metadata, nil
}

// getDeterministicCreatedTimestamp returns a deterministic timestamp for a chart version.
// This ensures that the same chart version always produces the same OCI manifest,
// making artifacts reproducible and fixing FluxCD reconciliation issues.
func getDeterministicCreatedTimestamp(chartVer *repo.ChartVersion) time.Time {
	// If the chart version has a Created timestamp from the index.yaml, use it
	if !chartVer.Created.IsZero() {
		return chartVer.Created
	}

	// Fallback: derive a deterministic timestamp from chart name and version
	// Use a fixed base timestamp and add deterministic offset based on chart metadata
	baseTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// Create a hash from chart name and version for deterministic offset
	hash := sha256.Sum256([]byte(chartVer.Name + "@" + chartVer.Version))

	// Use first 4 bytes of hash to create offset in seconds (max ~136 years)
	offset := int64(hash[0])<<24 | int64(hash[1])<<16 | int64(hash[2])<<8 | int64(hash[3])

	return baseTime.Add(time.Duration(offset) * time.Second)
}

func (m *Manifests) prepareChart(ctx context.Context, repo string, reference string, rewriteOpts RewriteOptions) *errors.RegError {
	elem := strings.Split(repo, "/")

	if len(elem) < 2 {
		return errors.RegErrInternal(fmt.Errorf("invalid repo length"))
	}

	path := strings.Join(elem[:len(elem)-1], "/")
	chart := elem[len(elem)-1]

	index, err := m.GetIndex(path)
	if err != nil {
		return &errors.RegError{
			Status:  http.StatusNotFound,
			Code:    "NAME_UNKNOWN",
			Message: fmt.Sprintf("index file fetch error: %s", path),
		}
	}

	if reference != "" && !strings.HasPrefix(reference, "v") {
		reference = fmt.Sprintf("v%s", reference)
	}

	m.log.Printf("searching index for %s with reference %s\n", chart, reference)
	chartVer, err := index.Get(chart, reference)
	if err != nil {
		reference = helper.SemVerReplace(reference)
		chartVer, err = index.Get(chart, reference)
	}
	if err != nil {

		return &errors.RegError{
			Status:  http.StatusNotFound,
			Code:    "NOT FOUND",
			Message: fmt.Sprintf("Chart: %s version: %s not found: %v", chart, reference, err),
		}
	}

	if len(chartVer.URLs) == 0 {
		return &errors.RegError{
			Status:  http.StatusNotFound,
			Code:    "NOT FOUND",
			Message: fmt.Sprintf("Chart has no URLs"),
		}
	}
	reference = strings.TrimPrefix(chartVer.Version, "v")

	var downloadUrl string

	u, err := url.Parse(chartVer.URLs[0])
	if err != nil {
		return errors.RegErrInternal(err)
	}
	if u.IsAbs() {
		downloadUrl = u.String()
	} else {
		downloadUrl = fmt.Sprintf("https://%s/%s", path, chartVer.URLs[0])
	}

	manifestData, err := m.download(downloadUrl)
	if err != nil {
		return errors.RegErrInternal(err)
	}

	// Rewrite chart dependencies if enabled
	if rewriteOpts.Enabled && rewriteOpts.ProxyHost != "" {
		rewrittenData, result, rewriteErr := rewriteChartDependencies(manifestData, rewriteOpts.ProxyHost, m.log)
		if rewriteErr != nil {
			m.log.Printf("warning: failed to rewrite dependencies: %v", rewriteErr)
			// Continue with original data (fail open)
		} else if result.Modified {
			manifestData = rewrittenData
			if m.config.Debug {
				m.log.Printf("rewrote %d dependencies in chart", len(result.Dependencies))
			}
		}
	}

	// Set deterministic timestamp for OCI manifest to ensure reproducible artifacts
	deterministicTime := getDeterministicCreatedTimestamp(chartVer)
	packOpts := oras.PackOptions{
		ManifestAnnotations: map[string]string{
			ocispec.AnnotationCreated: deterministicTime.Format(time.RFC3339),
		},
	}
	memStore := memory.New()

	// Extract chart metadata from the tarball to populate the OCI config layer
	// This provides the Chart.yaml contents as required by the Helm OCI spec
	chartMeta, err := extractChartMeta(manifestData)
	if err != nil {
		m.log.Printf("warning: failed to extract chart metadata: %v, using empty config", err)
		chartMeta = nil
	}

	var configData []byte
	if chartMeta != nil {
		configData, err = json.Marshal(chartMeta)
		if err != nil {
			m.log.Printf("warning: failed to marshal chart metadata: %v, using empty config", err)
			configData = []byte("{}")
		}
	} else {
		configData = []byte("{}")
	}

	desc := ocispec.Descriptor{
		MediaType: helmregistry.ConfigMediaType,
		Digest:    digest.FromBytes(configData),
		Size:      int64(len(configData)),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "$config",
		},
	}

	err = memStore.Push(ctx, desc, bytes.NewReader(configData))
	if err != nil {
		return errors.RegErrInternal(err)
	}

	desc.Annotations = packOpts.ConfigAnnotations
	packOpts.ConfigDescriptor = &desc
	packOpts.PackImageManifest = true
	name := filepath.Clean(filepath.Base(downloadUrl))

	manifestFile := ocispec.Descriptor{
		MediaType: helmregistry.ChartLayerMediaType,
		Digest:    digest.FromBytes(manifestData),
		Size:      int64(len(manifestData)),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: name,
		},
	}

	err = memStore.Push(ctx, manifestFile, bytes.NewReader(manifestData))
	copyOptions := oras.DefaultCopyOptions
	copyOptions.Concurrency = 1

	root, err := oras.Pack(ctx, memStore, "", []ocispec.Descriptor{manifestFile}, packOpts)
	if err != nil {
		return errors.RegErrInternal(err)
	}
	if err = memStore.Tag(ctx, root, root.Digest.String()); err != nil {
		return errors.RegErrInternal(err)
	}

	var refs []string

	copyOptions.PreCopy = func(ctx context.Context, desc ocispec.Descriptor) error {
		if desc.MediaType == ocispec.MediaTypeImageManifest {
			// oci manifest
			for k, ref := range refs {
				desc.Annotations[fmt.Sprintf("%s%d", ProxyRefAnnotationPrefix, k)] = ref
			}
		} else {
			refs = append(refs, desc.Digest.String())
		}
		return nil
	}

	dst := NewInternalDst(fmt.Sprintf("%s/%s", path, chartVer.Name), m.blobHandler.(handler.BlobPutHandler), m)
	// push
	if reference == "" {
		err = oras.CopyGraph(ctx, memStore, dst, root, copyOptions.CopyGraphOptions)
	} else {
		_, err = oras.Copy(ctx, memStore, root.Digest.String(), dst, reference, copyOptions)
	}
	if err != nil {
		return errors.RegErrInternal(err)
	}
	return nil
}

func (m *Manifests) GetIndex(repoURLPath string) (*repo.IndexFile, error) {

	type cacheResp struct {
		c   *repo.IndexFile
		err error
	}

	c, ok := m.cache.Get(repoURLPath)

	if !ok || c == nil {
		// nothing in the cache
		res := &cacheResp{}
		res.c, res.err = m.downloadIndex(repoURLPath)

		var ttl = m.config.IndexCacheTTL
		if res.err != nil {
			// cache error too to avoid external resource exhausting
			ttl = m.config.IndexErrorCacheTTl
		}
		// Use higher cost for parsed IndexFile structs to ensure proper cache admission
		// See: https://github.com/container-registry/helm-charts-oci-proxy/issues/11
		m.cache.SetWithTTL(repoURLPath, res, 100000, ttl)
		return res.c, res.err
	}

	res, ok := c.(*cacheResp)
	if !ok {
		return nil, fmt.Errorf("internal error")
	}
	return res.c, res.err
}

func (m *Manifests) downloadIndex(repoURLPath string) (*repo.IndexFile, error) {
	url := fmt.Sprintf("https://%s/index.yaml", repoURLPath)
	if m.config.Debug {
		m.log.Printf("download index: %s\n", url)
	}
	data, err := m.getIndexBytes(url)
	if err != nil {
		return nil, err
	}
	i := repo.NewIndexFile()

	if len(data) == 0 {
		return i, repo.ErrEmptyIndexYaml
	}
	if err = yaml.UnmarshalStrict(data, i); err != nil {
		return nil, err
	}

	for _, cvs := range i.Entries {
		for idx := len(cvs) - 1; idx >= 0; idx-- {
			if cvs[idx] == nil {
				continue
			}
			if cvs[idx].APIVersion == "" {
				cvs[idx].APIVersion = chart.APIVersionV1
			}
			if err := cvs[idx].Validate(); err != nil {
				cvs = append(cvs[:idx], cvs[idx+1:]...)
			}
		}
	}
	i.SortEntries()
	if i.APIVersion == "" {
		return i, repo.ErrNoAPIVersion
	}
	return i, nil
}

func (m *Manifests) getIndexBytes(url string) ([]byte, error) {

	type cacheResp struct {
		c   []byte
		err error
	}

	c, ok := m.cache.Get(url)

	if !ok || c == nil {
		// nothing in the cache
		res := &cacheResp{}
		res.c, res.err = m.download(url)

		var ttl = m.config.IndexCacheTTL
		if res.err != nil {
			// cache error too to avoid external resource exhausting
			ttl = m.config.IndexErrorCacheTTl
		}
		// Use actual byte size as cost for proper cache admission
		cost := int64(len(res.c))
		if cost < 1 {
			cost = 1 // minimum cost for error entries
		}
		m.cache.SetWithTTL(url, res, cost, ttl)
		return res.c, res.err
	}

	res, ok := c.(*cacheResp)
	if !ok {
		return nil, fmt.Errorf("internal error")
	}
	return res.c, res.err

}

func (m *Manifests) download(url string) ([]byte, error) {
	if m.config.Debug {
		m.log.Printf("downloading : %s\n", url)
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
