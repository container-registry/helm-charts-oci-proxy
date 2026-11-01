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
	"net"
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
		return downloadError(fmt.Errorf("index file fetch error: %s: %w", path, err), "NAME_UNKNOWN")
	}

	reference = IndexReference(reference)

	m.log.Printf("searching index for %s with reference %s\n", chart, reference)
	chartVer, err := index.Get(chart, reference)
	if err != nil {
		return &errors.RegError{
			Status:  http.StatusNotFound,
			Code:    "MANIFEST_UNKNOWN",
			Message: fmt.Sprintf("Chart: %s version: %s not found: %v", chart, reference, err),
		}
	}

	if len(chartVer.URLs) == 0 {
		return &errors.RegError{
			Status:  http.StatusNotFound,
			Code:    "MANIFEST_UNKNOWN",
			Message: fmt.Sprintf("Chart has no URLs"),
		}
	}
	reference = strings.TrimPrefix(chartVer.Version, "v")

	u, err := ChartURL(path, chartVer)
	if err != nil {
		return errors.RegErrInternal(err)
	}
	chartPrepareAttempts.Add(1)
	// The chart already lives in an OCI registry; there is no tarball to convert.
	if u.Scheme == "oci" {
		return &errors.RegError{
			Status:  http.StatusNotFound,
			Code:    "MANIFEST_UNKNOWN",
			Message: fmt.Sprintf("chart %s version %s is published only as OCI artifact %s; pull it from there", chartVer.Name, chartVer.Version, chartVer.URLs[0]),
		}
	}
	downloadUrl := u.String()

	manifestData, err := m.download(ctx, downloadUrl)
	if err != nil {
		return downloadError(err, "MANIFEST_UNKNOWN")
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

	if rewriteOpts.Enabled && rewriteOpts.ProxyHost != "" {
		rewrittenData, result, rewriteErr := rewriteChartDependencies(manifestData, rewriteOpts.ProxyHost, m.log)
		if rewriteErr != nil {
			m.log.Printf("warning: failed to rewrite dependencies: %v", rewriteErr)
		} else if result.Modified {
			// The config must describe the archive that is served.
			if meta, err := extractChartMeta(rewrittenData); err != nil {
				m.log.Printf("warning: rewritten chart is unreadable, serving the original: %v", err)
			} else {
				manifestData, chartMeta = rewrittenData, meta
				if m.config.Debug {
					m.log.Printf("rewrote %d dependencies in chart", len(result.Dependencies))
				}
			}
		}
	}

	// Set deterministic timestamp for OCI manifest to ensure reproducible artifacts
	deterministicTime := getDeterministicCreatedTimestamp(chartVer)
	packOpts := oras.PackOptions{
		ManifestAnnotations: generateOCIAnnotations(chartMeta, deterministicTime),
	}
	memStore := memory.New()

	configData, err := json.Marshal(chartMeta)
	if err != nil {
		return errors.RegErrInternal(err)
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

	put, ok := m.blobHandler.(handler.BlobPutHandler)
	if !ok {
		return errors.RegErrInternal(fmt.Errorf("blob handler does not accept writes"))
	}
	dst := NewInternalDst(fmt.Sprintf("%s/%s", path, chartVer.Name), put, m)
	// The copy is memory to memory and has no I/O to wait on. Stopping it
	// halfway, by cancellation or the preparation deadline, would leave blobs
	// that no manifest references, so cleanup would never delete them.
	copyCtx := context.WithoutCancel(ctx)
	err = func() error {
		m.copyMu.RLock()
		defer m.copyMu.RUnlock()
		if reference == "" {
			return oras.CopyGraph(copyCtx, memStore, dst, root, copyOptions.CopyGraphOptions)
		}
		_, err := oras.Copy(copyCtx, memStore, root.Digest.String(), dst, reference, copyOptions)
		return err
	}()
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
		// Concurrent misses for one repository share a single download and parse.
		v, _, _ := m.indexes.Do(repoURLPath, func() (interface{}, error) {
			// A caller that missed just before an earlier flight finished.
			if cached, ok := m.cache.Get(repoURLPath); ok {
				if res, ok := cached.(*cacheResp); ok {
					return res, nil
				}
			}
			res := &cacheResp{}
			var size int
			res.c, size, res.err = m.downloadIndex(repoURLPath)
			ttl := m.config.IndexCacheTTL
			if res.err != nil {
				// cache error too to avoid external resource exhausting
				ttl = m.config.IndexErrorCacheTTl
			}
			m.cache.SetWithTTL(repoURLPath, res, IndexCost(size, res.err, m.config.IndexCacheMaxCost), ttl)
			// ristretto applies writes asynchronously; without waiting, a request
			// right after this flight ends would miss and parse the index again.
			if w, ok := m.cache.(interface{ Wait() }); ok {
				w.Wait()
			}
			return res, nil
		})
		res := v.(*cacheResp)
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

func (m *Manifests) downloadIndex(repoURLPath string) (*repo.IndexFile, int, error) {
	url := fmt.Sprintf("https://%s/index.yaml", repoURLPath)
	if m.config.Debug {
		m.log.Printf("download index: %s\n", url)
	}
	indexDownloads.Add(1)
	data, err := m.download(context.Background(), url)
	if err != nil {
		return nil, 0, err
	}
	idx, err := ParseIndex(data)
	if err != nil {
		// ParseIndex can return a populated index with ErrNoAPIVersion; caching
		// it at the error cost would hold an uncounted index.
		return nil, 0, err
	}
	return idx, len(data), nil
}

// errorEntryCost approximates a cached download error: the cache entry, the
// error value and its message.
const errorEntryCost = 512

// IndexCost is the cache cost of a parsed index downloaded as size bytes, or of
// a cached download error. Measured retained heap of a parsed index is 1.6x to
// 1.7x its YAML size (bitnami 27.5MB -> 44MiB, prometheus-community 6.6MB ->
// 9.8MiB); 2x keeps the estimate conservative. The cost is capped at maxCost so
// an index larger than the budget is not refused for its size alone and then
// downloaded on every request.
func IndexCost(size int, err error, maxCost int64) int64 {
	if err != nil {
		return errorEntryCost + int64(len(err.Error()))
	}
	c := max(2*int64(size), 1)
	if maxCost > 0 && c > maxCost {
		c = maxCost
	}
	return c
}

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

// ParseIndex decodes and normalises a chart repository index the way the proxy
// resolves chart versions from it.
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

func (m *Manifests) download(ctx context.Context, rawURL string) ([]byte, error) {
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
		return nil, &policyError{fmt.Sprintf("refusing non-https download URL %q", rawURL)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Drain a bounded amount so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &upstreamStatusError{url: rawURL, status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxDownloadSize {
		return nil, fmt.Errorf("upstream %s response exceeds %d bytes", rawURL, maxDownloadSize)
	}
	return data, nil
}

// maxDownloadSize bounds every upstream body held in memory. The largest index
// seen in production (bitnami) is 27.5MB, and helm refuses charts that
// decompress to more than 100MiB, so a larger body is never usable.
var maxDownloadSize int64 = 100 << 20

type upstreamStatusError struct {
	url    string
	status int
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream %s returned HTTP %d", e.url, e.status)
}

// downloadError maps a failed upstream download to the registry response.
// Missing content answers 404 with notFoundCode, a refusal by the proxy's own
// policy answers 403, and every other failure is the upstream's fault.
func downloadError(err error, notFoundCode string) *errors.RegError {
	var pe *policyError
	if stderrors.As(err, &pe) {
		return &errors.RegError{Status: http.StatusForbidden, Code: "DENIED", Message: err.Error()}
	}
	var se *upstreamStatusError
	if stderrors.As(err, &se) && (se.status == http.StatusNotFound || se.status == http.StatusGone) {
		return &errors.RegError{Status: http.StatusNotFound, Code: notFoundCode, Message: err.Error()}
	}
	var de *net.DNSError
	if stderrors.As(err, &de) && de.IsNotFound {
		return &errors.RegError{Status: http.StatusNotFound, Code: notFoundCode, Message: err.Error()}
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
	algo, want, ok := strings.Cut(chartVer.Digest, ":")
	if !ok {
		algo, want = "sha256", chartVer.Digest
	}
	if !strings.EqualFold(algo, "sha256") {
		return
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(want, got) {
		chartDigestMismatches.Add(1)
		m.log.Printf("warning: %s@%s: index digest %s does not match tarball sha256 %s", chartVer.Name, chartVer.Version, chartVer.Digest, got)
	}
}
