package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"sigs.k8s.io/yaml"
)

const (
	classOK       = "ok"
	classUpstream = "upstream-problem"
	classNA       = "not-applicable"
	classProxyBug = "proxy-bug"
)

// maxBody matches the proxy's cap on any upstream body; a larger chart is
// unusable anyway because helm refuses charts above 100MiB decompressed.
const maxBody = 100 << 20

// Evidence is the exchange that made a proxy check fail.
type Evidence struct {
	Step        string `json:"step"`
	Request     string `json:"request,omitempty"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body,omitempty"`
	Detail      string `json:"detail"`
}

type Result struct {
	RepoURL        string    `json:"repo_url"`
	Chart          string    `json:"chart"`
	Version        string    `json:"version"`
	Class          string    `json:"class"`
	Reason         string    `json:"reason,omitempty"`
	OCIRef         string    `json:"oci_ref,omitempty"`
	DownloadURL    string    `json:"download_url,omitempty"`
	UpstreamDigest string    `json:"upstream_digest,omitempty"`
	UpstreamSize   int64     `json:"upstream_size,omitempty"`
	IndexDigest    string    `json:"index_digest,omitempty"`
	ManifestDigest string    `json:"manifest_digest,omitempty"`
	LayerDigest    string    `json:"layer_digest,omitempty"`
	FirstFailure   string    `json:"first_failure,omitempty"`
	Evidence       *Evidence `json:"evidence,omitempty"`
	Seconds        float64   `json:"seconds"`
}

type checker struct {
	proxy      *url.URL
	c          *http.Client
	helm       string
	helmEnv    []string
	work       string
	timeout    time.Duration
	retryDelay time.Duration
	idx        *indexCache
}

func newChecker(proxy *url.URL, c *http.Client, helm, work string, timeout, retryDelay time.Duration) *checker {
	// Isolated helm homes: no registry logins, repositories or caches of the user leak in.
	env := append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(work, "helm", "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(work, "helm", "config"),
		"HELM_DATA_HOME="+filepath.Join(work, "helm", "data"),
	)
	return &checker{
		proxy: proxy, c: c, helm: helm, helmEnv: env, work: work, timeout: timeout, retryDelay: retryDelay,
		idx: &indexCache{c: c, work: work, m: map[string]*indexEntry{}},
	}
}

// upstream is what the chart repository serves directly, the reference the
// proxy's answers are compared with.
type upstream struct {
	repoPath    string
	cv          *repo.ChartVersion
	downloadURL string
	digest      string
	size        int64
	meta        *chart.Metadata
	// staleIndexDigest is set when index.yaml lists a digest the tarball does not have.
	staleIndexDigest bool
}

// check has a named result so the deferred duration lands in the returned value.
func (k *checker) check(j job) (r Result) {
	start := time.Now()
	r = Result{RepoURL: j.pkg.Repository.URL, Chart: j.pkg.Name, Version: j.version}
	defer func() { r.Seconds = time.Since(start).Seconds() }()

	if !strings.HasPrefix(r.RepoURL, "https://") {
		r.Class, r.Reason = classNA, "repository is not a classic https chart repository"
		return r
	}
	up, class, reason := k.upstreamCheck(j, false)
	if up != nil {
		r.Version = up.cv.Version
		r.DownloadURL, r.UpstreamDigest, r.UpstreamSize, r.IndexDigest = up.downloadURL, up.digest, up.size, up.cv.Digest
		r.OCIRef = fmt.Sprintf("oci://%s/%s/%s", k.proxy.Host, up.repoPath, up.cv.Name)
	}
	if class != "" {
		r.Class, r.Reason = class, reason
		return r
	}

	note := ""
	if up.staleIndexDigest {
		note = "upstream index digest does not match the tarball it serves"
	}
	ev, err := k.viaProxy(&r, up)
	if err == nil {
		r.Class, r.Reason = classOK, note
		return r
	}
	// The proxy caches 404s and index errors for INDEX_ERROR_CACHE_TTL; the
	// delay lets a transient failure expire before the second attempt.
	r.FirstFailure = err.Error()
	time.Sleep(k.retryDelay)
	ev, err = k.viaProxy(&r, up)
	if err == nil {
		r.Class, r.Reason = classOK, strings.TrimPrefix(note+"; passed on retry", "; ")
		return r
	}
	// A fresh index: the run-wide cached one would miss a version republished
	// at a new URL, which the proxy already serves.
	again, class, reason := k.upstreamCheck(j, true)
	switch {
	case class != "":
		r.Class, r.Reason = classUpstream, "upstream failed on recheck: "+reason
	case again.downloadURL != up.downloadURL || again.digest != up.digest:
		r.Class, r.Reason = classUpstream, fmt.Sprintf("upstream changed during the run: %s %s, then %s %s", up.downloadURL, up.digest, again.downloadURL, again.digest)
	default:
		if ev != nil && ev.Detail == "" {
			ev.Detail = err.Error()
		}
		r.Class, r.Reason, r.Evidence = classProxyBug, err.Error(), ev
	}
	return r
}

func repoPath(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil {
		return strings.TrimPrefix(repoURL, "https://")
	}
	return u.Host + strings.TrimSuffix(u.Path, "/")
}

func (k *checker) upstreamCheck(j job, freshIndex bool) (*upstream, string, string) {
	get := k.idx.get
	if freshIndex {
		get = k.idx.load
	}
	idx, err := get(j.pkg.Repository.URL)
	if err != nil {
		return nil, classUpstream, "index: " + err.Error()
	}
	cv, err := idx.Get(j.pkg.Name, j.version)
	if err != nil {
		return nil, classUpstream, fmt.Sprintf("index has no %s %s: %v", j.pkg.Name, j.version, err)
	}
	up := &upstream{repoPath: repoPath(j.pkg.Repository.URL), cv: cv}
	if len(cv.URLs) == 0 {
		return up, classUpstream, "index entry has no download URL"
	}
	dl, err := repo.ResolveReferenceURL("https://"+up.repoPath, cv.URLs[0])
	if err != nil {
		return up, classUpstream, "download URL: " + err.Error()
	}
	up.downloadURL = dl
	switch {
	case strings.HasPrefix(dl, "oci://"):
		return up, classNA, "index points to an OCI artifact; the proxy only converts tarballs"
	case !strings.HasPrefix(dl, "https://"):
		return up, classNA, "download URL is not https; the proxy refuses it by policy"
	}
	data, _, err := getRetry(k.c, dl, maxBody)
	if err != nil {
		return up, classUpstream, "tarball: " + err.Error()
	}
	sum := sha256.Sum256(data)
	up.digest, up.size = "sha256:"+hex.EncodeToString(sum[:]), int64(len(data))
	if want := strings.TrimPrefix(strings.ToLower(cv.Digest), "sha256:"); want != "" && want != hex.EncodeToString(sum[:]) {
		up.staleIndexDigest = true
	}
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		return up, classUpstream, "invalid chart archive: " + err.Error()
	}
	up.meta = ch.Metadata
	return up, "", ""
}

// viaProxy pulls the chart through the proxy over the registry API and with
// helm, and compares every answer with the upstream.
func (k *checker) viaProxy(r *Result, up *upstream) (*Evidence, error) {
	name := up.repoPath + "/" + up.cv.Name
	tag := strings.ReplaceAll(up.cv.Version, "+", "_")
	base := k.proxy.String() + "/v2/" + name

	body, pe, err := k.proxyGet("manifest", base+"/manifests/"+tag, ocispec.MediaTypeImageManifest, 4<<20)
	if err != nil {
		return pe.Evidence, err
	}
	mdigest := digestOf(body)
	r.ManifestDigest = mdigest
	if pe.ContentType != ocispec.MediaTypeImageManifest {
		return pe.Evidence, fmt.Errorf("manifest Content-Type %q", pe.ContentType)
	}
	if got := pe.dockerDigest; got != mdigest {
		return pe.Evidence, fmt.Errorf("Docker-Content-Digest %s, body digest %s", got, mdigest)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return pe.Evidence, fmt.Errorf("manifest: %w", err)
	}
	switch {
	case m.SchemaVersion != 2 || m.MediaType != ocispec.MediaTypeImageManifest:
		return pe.Evidence, fmt.Errorf("manifest schemaVersion %d mediaType %q", m.SchemaVersion, m.MediaType)
	case m.Config.MediaType != helmregistry.ConfigMediaType:
		return pe.Evidence, fmt.Errorf("config mediaType %q", m.Config.MediaType)
	case len(m.Layers) != 1:
		return pe.Evidence, fmt.Errorf("manifest has %d layers", len(m.Layers))
	case m.Layers[0].MediaType != helmregistry.ChartLayerMediaType:
		return pe.Evidence, fmt.Errorf("layer mediaType %q", m.Layers[0].MediaType)
	}
	layer := m.Layers[0]
	r.LayerDigest = layer.Digest.String()
	if layer.Digest.String() != up.digest || layer.Size != up.size {
		return pe.Evidence, fmt.Errorf("manifest layer %s (%d bytes), upstream tarball %s (%d bytes)", layer.Digest, layer.Size, up.digest, up.size)
	}

	byDigest, pe, err := k.proxyGet("manifest-by-digest", base+"/manifests/"+mdigest, ocispec.MediaTypeImageManifest, 4<<20)
	if err != nil {
		return pe.Evidence, err
	}
	if !bytes.Equal(byDigest, body) {
		return pe.Evidence, fmt.Errorf("manifest by digest differs from manifest by tag")
	}

	cfg, pe, err := k.proxyGet("config-blob", base+"/blobs/"+m.Config.Digest.String(), "", 4<<20)
	if err != nil {
		return pe.Evidence, err
	}
	if d := digestOf(cfg); d != m.Config.Digest.String() || int64(len(cfg)) != m.Config.Size {
		return pe.Evidence, fmt.Errorf("config blob %s (%d bytes), manifest says %s (%d bytes)", d, len(cfg), m.Config.Digest, m.Config.Size)
	}
	var meta chart.Metadata
	if err := json.Unmarshal(cfg, &meta); err != nil {
		return pe.Evidence, fmt.Errorf("config blob: %w", err)
	}
	if meta.Name != up.meta.Name || meta.Version != up.meta.Version {
		return pe.Evidence, fmt.Errorf("config says %s %s, Chart.yaml says %s %s", meta.Name, meta.Version, up.meta.Name, up.meta.Version)
	}

	if ev, err := k.layer(base+"/blobs/"+layer.Digest.String(), up); err != nil {
		return ev, err
	}
	return k.helmChecks(r.OCIRef, up)
}

func (k *checker) proxyGet(step, u, accept string, limit int64) ([]byte, *proxyEvidenceRef, error) {
	ev := &proxyEvidenceRef{Evidence: &Evidence{Step: step, Request: "GET " + u}}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, ev, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := k.c.Do(req)
	if err != nil {
		ev.Detail = err.Error()
		return nil, ev, fmt.Errorf("%s: %w", step, err)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	ev.Status, ev.ContentType, ev.dockerDigest = resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Docker-Content-Digest")
	ev.Body = printable(body)
	if resp.StatusCode != http.StatusOK {
		ev.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return nil, ev, fmt.Errorf("%s: HTTP %d: %s", step, resp.StatusCode, oneLine(ev.Body, 300))
	}
	if rerr != nil {
		ev.Detail = rerr.Error()
		return nil, ev, fmt.Errorf("%s: reading body: %w", step, rerr)
	}
	if int64(len(body)) > limit {
		ev.Detail = "body too large"
		return nil, ev, fmt.Errorf("%s: body exceeds %d bytes", step, limit)
	}
	return body, ev, nil
}

// proxyEvidenceRef carries response headers the comparison needs next to the
// evidence that ends up in the report.
type proxyEvidenceRef struct {
	*Evidence
	dockerDigest string
}

func (k *checker) layer(u string, up *upstream) (*Evidence, error) {
	ev := &Evidence{Step: "layer-blob", Request: "GET " + u}
	resp, err := k.c.Get(u)
	if err != nil {
		ev.Detail = err.Error()
		return ev, fmt.Errorf("layer blob: %w", err)
	}
	defer resp.Body.Close()
	ev.Status, ev.ContentType = resp.StatusCode, resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ev.Body, ev.Detail = printable(b), fmt.Sprintf("HTTP %d", resp.StatusCode)
		return ev, fmt.Errorf("layer blob: HTTP %d: %s", resp.StatusCode, oneLine(ev.Body, 300))
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, maxBody+1))
	d := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if err != nil {
		ev.Detail = fmt.Sprintf("stream broke after %d bytes: %v", n, err)
		return ev, fmt.Errorf("layer blob: %s", ev.Detail)
	}
	if d != up.digest || n != up.size {
		ev.Detail = fmt.Sprintf("served %s (%d bytes), upstream %s (%d bytes)", d, n, up.digest, up.size)
		return ev, fmt.Errorf("layer blob: %s", ev.Detail)
	}
	return nil, nil
}

func (k *checker) helmChecks(ref string, up *upstream) (*Evidence, error) {
	dest, err := os.MkdirTemp(k.work, "pull-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dest)
	args := []string{"pull", ref, "--version", up.cv.Version, "--destination", dest}
	if _, ev, err := k.helmRun("helm-pull", args...); err != nil {
		return ev, err
	}
	files, _ := filepath.Glob(filepath.Join(dest, "*.tgz"))
	ev := &Evidence{Step: "helm-pull", Request: "helm " + strings.Join(args, " ")}
	if len(files) != 1 {
		ev.Detail = fmt.Sprintf("%d .tgz files written", len(files))
		return ev, fmt.Errorf("helm pull: %s", ev.Detail)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		return ev, err
	}
	if d := digestOf(data); d != up.digest {
		ev.Detail = fmt.Sprintf("pulled %s %s, upstream %s", filepath.Base(files[0]), d, up.digest)
		return ev, fmt.Errorf("helm pull: %s", ev.Detail)
	}

	out, ev, err := k.helmRun("helm-show-chart", "show", "chart", ref, "--version", up.cv.Version)
	if err != nil {
		return ev, err
	}
	var meta chart.Metadata
	if err := yaml.Unmarshal(out, &meta); err != nil {
		ev.Detail, ev.Body = err.Error(), printable(out)
		return ev, fmt.Errorf("helm show chart: %w", err)
	}
	if meta.Name != up.meta.Name || meta.Version != up.meta.Version {
		ev.Detail = fmt.Sprintf("shows %s %s, Chart.yaml says %s %s", meta.Name, meta.Version, up.meta.Name, up.meta.Version)
		return ev, fmt.Errorf("helm show chart: %s", ev.Detail)
	}
	return nil, nil
}

func (k *checker) helmRun(step string, args ...string) ([]byte, *Evidence, error) {
	if k.proxy.Scheme == "http" {
		args = append(args, "--plain-http")
	}
	ev := &Evidence{Step: step, Request: "helm " + strings.Join(args, " ")}
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.helm, args...)
	cmd.Env = k.helmEnv
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		ev.Detail, ev.Body = err.Error(), printable(stderr.Bytes())
		return out, ev, fmt.Errorf("%s: %v: %s", step, err, oneLine(ev.Body, 300))
	}
	return out, ev, nil
}

type statusError struct {
	url    string
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d %s", e.url, e.status, oneLine(e.body, 200))
}

func getBytes(c *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &statusError{url: u, status: resp.StatusCode, body: printable(b)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", u, limit)
	}
	return data, nil
}

// getRetry repeats a failed GET once unless the upstream said the content is gone.
func getRetry(c *http.Client, u string, limit int64) ([]byte, int, error) {
	data, err := getBytes(c, u, limit)
	var se *statusError
	if err != nil && !(errors.As(err, &se) && (se.status == http.StatusNotFound || se.status == http.StatusGone)) {
		time.Sleep(5 * time.Second)
		data, err = getBytes(c, u, limit)
	}
	if errors.As(err, &se) {
		return nil, se.status, err
	}
	return data, 0, err
}

type indexEntry struct {
	once sync.Once
	idx  *repo.IndexFile
	err  error
}

// indexCache fetches every index.yaml once per run, directly from the
// repository, and parses it with helm's own loader as `helm repo add` would.
type indexCache struct {
	c    *http.Client
	work string
	mu   sync.Mutex
	m    map[string]*indexEntry
}

func (ic *indexCache) get(repoURL string) (*repo.IndexFile, error) {
	ic.mu.Lock()
	e, ok := ic.m[repoURL]
	if !ok {
		e = &indexEntry{}
		ic.m[repoURL] = e
	}
	ic.mu.Unlock()
	e.once.Do(func() { e.idx, e.err = ic.load(repoURL) })
	return e.idx, e.err
}

func (ic *indexCache) load(repoURL string) (*repo.IndexFile, error) {
	data, _, err := getRetry(ic.c, strings.TrimSuffix(repoURL, "/")+"/index.yaml", maxBody)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(ic.work, "index-*.yaml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return repo.LoadIndexFile(f.Name())
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func printable(b []byte) string {
	if len(b) > 2048 {
		b = b[:2048]
	}
	if !utf8.Valid(b) {
		return fmt.Sprintf("<%d bytes, not text>", len(b))
	}
	return string(b)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}
