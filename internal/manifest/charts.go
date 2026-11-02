package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	stderrors "errors"
	"expvar"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/container-registry/helm-charts-oci-proxy/internal/helper"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/repo"
	"sigs.k8s.io/yaml"
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

// ManifestTarget turns a requested manifest tag into the key the proxy caches
// manifests under: Helm's "_" form of build metadata becomes "+". A leading "v"
// is kept, because an index can list 1.0.0 and v1.0.0 as different charts.
func ManifestTarget(tag string) string {
	return helper.TagToVersion(tag)
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

func (m *Manifests) prepareChart(ctx context.Context, repo string, reference string) *errors.RegError {
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

	m.log.Printf("searching index for %s with reference %s\n", chart, reference)
	chartVer, err := ResolveVersion(index, chart, reference)
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
	// Both store paths below key the manifest by the index version; a tag that
	// named it with the leading "v" added or removed is aliased to it.
	if reference != "" && reference != chartVer.Version && toggleV(reference) == chartVer.Version {
		repoKey := path + "/" + chartVer.Name
		if m.writeAlias(repoKey, chartVer.Version, reference) {
			return nil
		}
		defer m.writeAlias(repoKey, chartVer.Version, reference)
	}
	reference = chartVer.Version

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

	proxyHost := m.config.ProxyHost
	// Dependency rewriting changes the tarball, so only that path buffers it.
	if !m.config.RewriteDependencies || proxyHost == "" {
		return m.prepareStreamed(ctx, path, chartVer, downloadUrl, reference)
	}

	manifestData, err := m.download(ctx, downloadUrl)
	if err != nil {
		return downloadError(err, "MANIFEST_UNKNOWN")
	}
	// Some hosts answer 200 with an HTML page for missing files; packaging that
	// would hand clients a chart that cannot be installed.
	if !isGzip(manifestData) {
		return upstreamError("upstream %s did not return a gzip chart archive", downloadUrl)
	}
	m.checkIndexDigest(chartVer, digest.FromBytes(manifestData).Encoded())

	// A gzip header alone does not make a chart: a truncated archive or
	// gzip-compressed HTML passes isGzip but cannot be installed.
	chartMeta, err := extractChartMeta(manifestData)
	if err != nil {
		return upstreamError("upstream %s did not return a valid chart archive: %v", downloadUrl, err)
	}

	rewrittenData, result, rewriteErr := rewriteChartDependencies(manifestData, proxyHost, m.log)
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

	layer := layerDescriptor(downloadUrl, digest.FromBytes(manifestData), int64(len(manifestData)))
	return m.storeChart(ctx, path, chartVer, reference, chartMeta, layer, manifestData, "")
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
				ttl = m.config.IndexErrorCacheTTL
			}
			// ristretto keeps an entry with TTL 0 forever; for errors 0 means
			// do not cache.
			if res.err == nil || ttl > 0 {
				m.cache.SetWithTTL(repoURLPath, res, IndexCost(size, res.err, m.config.IndexCacheMaxCost), ttl)
			}
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

// openUpstream starts a GET to an upstream chart repository and returns the
// body of a 2xx response.
func (m *Manifests) openUpstream(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	return m.get(ctx, m.httpClient, rawURL)
}

// openLayer streams a chart layer to a client; see newLayerClient.
func (m *Manifests) openLayer(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	return m.get(ctx, m.layerClient, rawURL)
}

func (m *Manifests) get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
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
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Drain a bounded amount so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, &upstreamStatusError{url: rawURL, status: resp.StatusCode}
	}
	return resp.Body, nil
}

func (m *Manifests) download(ctx context.Context, rawURL string) ([]byte, error) {
	body, err := m.openUpstream(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxDownloadSize {
		return nil, fmt.Errorf("upstream %s response exceeds %d bytes", rawURL, maxDownloadSize)
	}
	return data, nil
}

// maxDownloadSize bounds every upstream body. The largest index seen in
// production (bitnami) is 27.5MB, and helm refuses charts that decompress to
// more than 100MiB, so a larger body is never usable.
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
func (m *Manifests) checkIndexDigest(chartVer *repo.ChartVersion, got string) {
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
	if !strings.EqualFold(want, got) {
		chartDigestMismatches.Add(1)
		m.log.Printf("warning: %s@%s: index digest %s does not match tarball sha256 %s", chartVer.Name, chartVer.Version, chartVer.Digest, got)
	}
}
