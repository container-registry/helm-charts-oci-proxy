package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"expvar"
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

// generateOCIAnnotations mirrors Helm's generateOCIAnnotations() behavior,
// producing OCI image annotations from chart metadata.
// Order matches Helm: standard annotations first, then custom annotations
// from Chart.yaml can override everything except title and version.
// If meta is nil, it returns a map with only the created annotation.
func generateOCIAnnotations(meta *chart.Metadata, created time.Time) map[string]string {
	annotations := map[string]string{
		ocispec.AnnotationCreated: created.Format(time.RFC3339),
	}
	if meta == nil {
		return annotations
	}

	// Standard OCI annotations first
	annotations[ocispec.AnnotationTitle] = meta.Name
	annotations[ocispec.AnnotationVersion] = meta.Version

	if meta.Description != "" {
		annotations[ocispec.AnnotationDescription] = meta.Description
	}
	if meta.Home != "" {
		annotations[ocispec.AnnotationURL] = meta.Home
	}
	if len(meta.Sources) > 0 {
		annotations[ocispec.AnnotationSource] = meta.Sources[0]
	}
	if len(meta.Maintainers) > 0 {
		var parts []string
		for _, m := range meta.Maintainers {
			if m.Email != "" {
				parts = append(parts, fmt.Sprintf("%s (%s)", m.Name, m.Email))
			} else {
				parts = append(parts, m.Name)
			}
		}
		annotations[ocispec.AnnotationAuthors] = strings.Join(parts, ", ")
	}

	// Custom annotations from Chart.yaml override standard ones, except title and version
	for k, v := range meta.Annotations {
		if k == ocispec.AnnotationTitle || k == ocispec.AnnotationVersion {
			continue
		}
		annotations[k] = v
	}

	return annotations
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

// ManifestTarget turns a requested manifest tag into the version key the
// proxy caches manifests under: a leading "v" is dropped and Helm's "_" form of
// build metadata becomes "+".
func ManifestTarget(tag string) string {
	return helper.TagToVersion(strings.TrimPrefix(tag, "v"))
}

// IndexReference is the reference prepareChart looks up in the index for a
// manifest target.
func IndexReference(target string) string {
	if target == "" || strings.HasPrefix(target, "v") {
		return target
	}
	return "v" + target
}

// ChartURL resolves the download URL of a chart version the way helm's
// repo.ResolveReferenceURL does, so root- and parent-relative entries work.
func ChartURL(indexPath string, cv *repo.ChartVersion) (*url.URL, error) {
	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("chart %s version %s has no URLs", cv.Name, cv.Version)
	}
	resolved, err := repo.ResolveReferenceURL("https://"+indexPath, cv.URLs[0])
	if err != nil {
		return nil, err
	}
	return url.Parse(resolved)
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

	reference = IndexReference(reference)

	m.log.Printf("searching index for %s with reference %s\n", chart, reference)
	chartVer, err := index.Get(chart, reference)
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

	u, err := ChartURL(path, chartVer)
	if err != nil {
		return errors.RegErrInternal(err)
	}
	downloadUrl := u.String()

	chartPrepareAttempts.Add(1)
	manifestData, err := m.download(downloadUrl)
	if err != nil {
		return chartDownloadError(err)
	}
	// Some hosts answer 200 with an HTML page for missing files; packaging that
	// would hand clients a chart that cannot be installed.
	if !isGzip(manifestData) {
		return &errors.RegError{
			Status:  http.StatusBadGateway,
			Code:    "UPSTREAM_ERROR",
			Message: fmt.Sprintf("upstream %s did not return a gzip chart archive", downloadUrl),
		}
	}
	m.checkIndexDigest(chartVer, manifestData)

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

	// Extract chart metadata from the tarball to populate the OCI config layer
	// This provides the Chart.yaml contents as required by the Helm OCI spec
	// A gzip header alone does not make a chart: a truncated archive or
	// gzip-compressed HTML passes isGzip but cannot be installed.
	chartMeta, err := extractChartMeta(manifestData)
	if err != nil {
		return &errors.RegError{
			Status:  http.StatusBadGateway,
			Code:    "UPSTREAM_ERROR",
			Message: fmt.Sprintf("upstream %s did not return a valid chart archive: %v", downloadUrl, err),
		}
	}

	// Set deterministic timestamp for OCI manifest to ensure reproducible artifacts
	deterministicTime := getDeterministicCreatedTimestamp(chartVer)
	packOpts := oras.PackOptions{
		ManifestAnnotations: generateOCIAnnotations(chartMeta, deterministicTime),
	}
	memStore := memory.New()

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
	chartsPrepared.Add(1)
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

var (
	indexParses           = expvar.NewInt("index_parses")
	indexParseMs          = expvar.NewInt("index_parse_ms")
	indexDownloads        = expvar.NewInt("index_downloads")
	chartsPrepared        = expvar.NewInt("charts_prepared")
	chartPrepareAttempts  = expvar.NewInt("chart_prepare_attempts")
	chartDigestMismatches = expvar.NewInt("chart_digest_mismatches")
)

func (m *Manifests) downloadIndex(repoURLPath string) (*repo.IndexFile, error) {
	url := fmt.Sprintf("https://%s/index.yaml", repoURLPath)
	if m.config.Debug {
		m.log.Printf("download index: %s\n", url)
	}
	data, err := m.getIndexBytes(url)
	if err != nil {
		return nil, err
	}
	return ParseIndex(data)
}

// ParseIndex decodes and normalises a chart repository index the way the proxy
// resolves chart versions from it.
// ignoreSkippableChartValidationError is unexported in helm's repo package;
// this copy keeps the same exception (helm/helm#12748: JFrog strips aliases).
func ignoreSkippableChartValidationError(err error) error {
	var verr chart.ValidationError
	if !stderrors.As(err, &verr) {
		return err
	}
	if strings.HasPrefix(verr.Error(), "validation: more than one dependency with name or alias") {
		return nil
	}
	return err
}

func ParseIndex(data []byte) (*repo.IndexFile, error) {
	indexParses.Add(1)
	defer func(t time.Time) { indexParseMs.Add(time.Since(t).Milliseconds()) }(time.Now())
	i := repo.NewIndexFile()

	if len(data) == 0 {
		return i, repo.ErrEmptyIndexYaml
	}
	if err := yaml.UnmarshalStrict(data, i); err != nil {
		return nil, err
	}

	// Mirrors helm's repo.loadIndex, so the proxy offers the same versions as
	// `helm repo add` would.
	for name, cvs := range i.Entries {
		for idx := len(cvs) - 1; idx >= 0; idx-- {
			if cvs[idx] == nil {
				cvs = append(cvs[:idx], cvs[idx+1:]...)
				continue
			}
			if cvs[idx].Metadata == nil {
				cvs[idx].Metadata = &chart.Metadata{}
			}
			if cvs[idx].APIVersion == "" {
				cvs[idx].APIVersion = chart.APIVersionV1
			}
			if err := cvs[idx].Validate(); ignoreSkippableChartValidationError(err) != nil {
				cvs = append(cvs[:idx], cvs[idx+1:]...)
			}
		}
		i.Entries[name] = cvs
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
		indexDownloads.Add(1)
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

func (m *Manifests) download(rawURL string) ([]byte, error) {
	if m.config.Debug {
		m.log.Printf("downloading : %s\n", rawURL)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	// index.yaml can point anywhere; plain http would undermine both integrity
	// and the private-network dial guard.
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-https download URL %q", rawURL)
	}
	resp, err := m.httpClient.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Drain a bounded amount so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &upstreamStatusError{url: rawURL, status: resp.StatusCode}
	}
	return io.ReadAll(resp.Body)
}

type upstreamStatusError struct {
	url    string
	status int
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream %s returned HTTP %d", e.url, e.status)
}

// chartDownloadError maps a failed chart download to the registry response.
// An upstream 404 or 410 means the chart version does not exist; any other
// upstream failure is the upstream's fault, not the proxy's.
func chartDownloadError(err error) *errors.RegError {
	var se *upstreamStatusError
	if !stderrors.As(err, &se) {
		// Dial, TLS, timeout and body-read failures are upstream failures too.
		return &errors.RegError{Status: http.StatusBadGateway, Code: "UPSTREAM_ERROR", Message: err.Error()}
	}
	if se.status == http.StatusNotFound || se.status == http.StatusGone {
		return &errors.RegError{Status: http.StatusNotFound, Code: "MANIFEST_UNKNOWN", Message: err.Error()}
	}
	return &errors.RegError{Status: http.StatusBadGateway, Code: "UPSTREAM_ERROR", Message: err.Error()}
}

func isGzip(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

// checkIndexDigest compares the tarball with the digest published in
// index.yaml. A mismatch is only reported: Helm does not verify this digest on
// pull, and about 7% of versions in large public indexes carry a stale one
// while their tarball is fine.
func (m *Manifests) checkIndexDigest(chartVer *repo.ChartVersion, data []byte) {
	if chartVer.Digest == "" {
		return
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(strings.TrimPrefix(chartVer.Digest, "sha256:"), got) {
		chartDigestMismatches.Add(1)
		m.log.Printf("warning: %s@%s: index digest %s does not match tarball sha256 %s", chartVer.Name, chartVer.Version, chartVer.Digest, got)
	}
}
