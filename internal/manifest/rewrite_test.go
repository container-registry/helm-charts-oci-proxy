package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"helm.sh/helm/v3/pkg/chart"
	"sigs.k8s.io/yaml"
)

func TestShouldRewriteURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{"empty URL", "", false},
		{"https URL", "https://charts.bitnami.com/bitnami", true},
		{"http URL", "http://charts.example.com/charts", true},
		{"oci URL", "oci://registry.example.com/charts", true},
		{"file URL", "file://./charts/mychart", false},
		{"file URL no double slash", "file:./charts/mychart", false},
		{"alias with @", "@bitnami", false},
		{"alias with prefix", "alias:bitnami", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldRewriteURL(tt.url)
			if result != tt.expected {
				t.Errorf("shouldRewriteURL(%q) = %v, want %v", tt.url, result, tt.expected)
			}
		})
	}
}

func TestRewriteDependencyURL(t *testing.T) {
	proxyHost := "chartproxy.example.com"

	tests := []struct {
		name        string
		originalURL string
		proxyHost   string
		wantURL     string
		wantOCI     bool
		wantErr     bool
	}{
		{
			name:        "https URL with path",
			originalURL: "https://charts.bitnami.com/bitnami",
			proxyHost:   proxyHost,
			wantURL:     "oci://chartproxy.example.com/charts.bitnami.com/bitnami",
			wantOCI:     false,
			wantErr:     false,
		},
		{
			name:        "https URL without path",
			originalURL: "https://charts.jetstack.io",
			proxyHost:   proxyHost,
			wantURL:     "oci://chartproxy.example.com/charts.jetstack.io",
			wantOCI:     false,
			wantErr:     false,
		},
		{
			name:        "http URL",
			originalURL: "http://charts.example.com/repo",
			proxyHost:   proxyHost,
			wantURL:     "oci://chartproxy.example.com/charts.example.com/repo",
			wantOCI:     false,
			wantErr:     false,
		},
		{
			name:        "oci URL",
			originalURL: "oci://registry.example.com/charts/mychart",
			proxyHost:   proxyHost,
			wantURL:     "oci://chartproxy.example.com/registry.example.com/charts/mychart",
			wantOCI:     true,
			wantErr:     false,
		},
		{
			name:        "empty URL",
			originalURL: "",
			proxyHost:   proxyHost,
			wantErr:     true,
		},
		{
			name:        "unsupported scheme",
			originalURL: "ftp://charts.example.com",
			proxyHost:   proxyHost,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotOCI, err := rewriteDependencyURL(tt.originalURL, tt.proxyHost)
			if (err != nil) != tt.wantErr {
				t.Errorf("rewriteDependencyURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if gotURL != tt.wantURL {
					t.Errorf("rewriteDependencyURL() gotURL = %v, want %v", gotURL, tt.wantURL)
				}
				if gotOCI != tt.wantOCI {
					t.Errorf("rewriteDependencyURL() gotOCI = %v, want %v", gotOCI, tt.wantOCI)
				}
			}
		})
	}
}

// createTestTarball creates a gzipped tarball with a Chart.yaml for testing
func createTestTarball(chartName string, metadata *chart.Metadata) ([]byte, error) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Marshal Chart.yaml
	chartYAML, err := yaml.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	// Add Chart.yaml
	chartYAMLPath := chartName + "/Chart.yaml"
	header := &tar.Header{
		Name: chartYAMLPath,
		Mode: 0644,
		Size: int64(len(chartYAML)),
	}
	if err := tw.WriteHeader(header); err != nil {
		return nil, err
	}
	if _, err := tw.Write(chartYAML); err != nil {
		return nil, err
	}

	// Add a dummy values.yaml
	valuesYAML := []byte("key: value\n")
	valuesPath := chartName + "/values.yaml"
	header = &tar.Header{
		Name: valuesPath,
		Mode: 0644,
		Size: int64(len(valuesYAML)),
	}
	if err := tw.WriteHeader(header); err != nil {
		return nil, err
	}
	if _, err := tw.Write(valuesYAML); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Only http(s) and oci dependency repositories are pointed at the proxy;
// local files and repository aliases are kept.
func TestRewriteChart(t *testing.T) {
	const proxied = "oci://chartproxy.example.com/charts.example.com/stable"
	for _, tc := range []struct {
		name     string
		deps     []*chart.Dependency
		modified bool
		want     map[string]string
	}{
		{"no dependencies", nil, false, map[string]string{}},
		{"remote dependencies", []*chart.Dependency{
			{Name: "redis", Version: "17.0.0", Repository: "https://charts.example.com/stable"},
			{Name: "postgresql", Version: "12.0.0", Repository: "https://charts.example.com/stable"},
		}, true, map[string]string{"redis": proxied, "postgresql": proxied}},
		{"mixed dependencies", []*chart.Dependency{
			{Name: "redis", Version: "17.0.0", Repository: "https://charts.example.com/stable"},
			{Name: "local-chart", Version: "1.0.0", Repository: "file://../local-chart"},
			{Name: "aliased-chart", Version: "1.0.0", Repository: "@myrepo"},
		}, true, map[string]string{"redis": proxied, "local-chart": "file://../local-chart", "aliased-chart": "@myrepo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tgz, err := createTestTarball("test-chart", &chart.Metadata{APIVersion: "v2", Name: "test-chart", Version: "1.0.0", Dependencies: tc.deps})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			modified, err := rewriteChart(&out, bytes.NewReader(tgz), "chartproxy.example.com", log.New(io.Discard, "", 0))
			if err != nil || modified != tc.modified {
				t.Fatalf("rewriteChart: modified %v, %v; want %v", modified, err, tc.modified)
			}
			got := map[string]string{}
			for _, d := range chartYAMLs(t, out.Bytes())["test-chart/Chart.yaml"].Dependencies {
				got[d.Name] = d.Repository
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("repositories = %v, want %v", got, tc.want)
			}
		})
	}
}

// chartYAMLs parses every Chart.yaml in a chart archive, keyed by entry name.
func chartYAMLs(t *testing.T, tgz []byte) map[string]*chart.Metadata {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*chart.Metadata{}
	tr := tar.NewReader(zr)
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(hd.Name, "/Chart.yaml") {
			data, _ := io.ReadAll(tr)
			var md chart.Metadata
			if err := yaml.Unmarshal(data, &md); err != nil {
				t.Fatal(err)
			}
			out[hd.Name] = &md
		}
	}
}

func chartWithDependency(name, repository string) string {
	return fmt.Sprintf("apiVersion: v2\nname: %s\nversion: 1.0.0\ndependencies:\n  - name: dep\n    version: 1.0.0\n    repository: %s\n", name, repository)
}

// rewritingFixture serves a chart whose own and subchart Chart.yaml both list
// a dependency, with the subchart first in the archive.
func rewritingFixture(t *testing.T) *streamFixture {
	t.Helper()
	f := newStreamFixture(t)
	f.m.config.RewriteDependencies = true
	f.m.config.ProxyHost = "proxy.example.com"
	f.serve.Store(tarballOf(t, [][2]string{
		{"demo/charts/sub/Chart.yaml", chartWithDependency("sub", "https://sub.example.com/charts")},
		{"demo/Chart.yaml", chartWithDependency("demo", "https://top.example.com/charts")},
	}))
	return f
}

func readLayer(t *testing.T, f *streamFixture, d digest.Digest) []byte {
	t.Helper()
	rc, err := f.open(t, d)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("layer read: %v", err)
	}
	return data
}

// Only the chart's own Chart.yaml is rewritten. A subchart's charts/x/Chart.yaml
// also ends in /Chart.yaml; when it came first in the archive, it was
// rewritten instead and the chart's own dependencies were left alone.
func TestRewrite_TopLevelChartYAMLOnly(t *testing.T) {
	f := rewritingFixture(t)
	got := chartYAMLs(t, readLayer(t, f, f.layer(t)))
	if r := got["demo/Chart.yaml"].Dependencies[0].Repository; r != "oci://proxy.example.com/top.example.com/charts" {
		t.Errorf("chart dependency repository = %s, not rewritten", r)
	}
	if r := got["demo/charts/sub/Chart.yaml"].Dependencies[0].Repository; r != "https://sub.example.com/charts" {
		t.Errorf("subchart dependency repository = %s, want it unchanged", r)
	}
}

// A rewritten layer is not stored: every blob GET rewrites the upstream
// tarball again, so the rewrite must give the same bytes each time it runs.
func TestRewrite_LayerIsRewrittenOnEveryRead(t *testing.T) {
	f := rewritingFixture(t)
	d := f.layer(t)
	if b, _ := f.m.findBlob(f.repo, d); b.data != nil {
		t.Fatalf("the rewritten tarball (%d bytes) is held in memory", len(b.data))
	}
	first := readLayer(t, f, d)
	second := readLayer(t, f, d)
	if got := digest.FromBytes(first); got != d || !bytes.Equal(first, second) {
		t.Fatalf("layer reads %s and %s, manifest says %s", got, digest.FromBytes(second), d)
	}
	if n := f.downloads.Load(); n != 3 {
		t.Errorf("%d upstream downloads, want 3 (manifest, then each read)", n)
	}
	if other := rewritingFixture(t).layer(t); other != d {
		t.Errorf("two preparations of one tarball gave layers %s and %s", d, other)
	}
}

// A chart helm rejects is rejected with rewriting on as well, while the rewrite
// still streams the rest of the archive: one download, no fallback.
func TestRewrite_InvalidChartFailsCleanly(t *testing.T) {
	noise := make([]byte, 256<<10)
	_, _ = rand.Read(noise)
	f := newStreamFixture(t)
	f.m.config.RewriteDependencies = true
	f.m.config.ProxyHost = "proxy.example.com"
	var logs bytes.Buffer
	f.m.log = log.New(&logs, "", 0)
	f.serve.Store(tarballOf(t, [][2]string{
		{"demo/Chart.yaml", chartWithDependency("demo", "https://top.example.com/charts")},
		{"demo/values.yaml", "a: [\n"},
		{"demo/templates/noise.txt", string(noise)},
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+f.repo+"/manifests/1.0.0", nil)
	if err := f.m.Handle(rec, req); err == nil {
		t.Fatal("a chart helm rejects was prepared")
	}
	if n := f.downloads.Load(); n != 1 {
		t.Errorf("%d downloads of a chart helm rejects, want 1", n)
	}
	if strings.Contains(logs.String(), "failed to rewrite") {
		t.Errorf("an invalid chart was logged as a rewrite failure: %s", logs.String())
	}
}

// blockingSource yields a gzip header, then blocks until closed and reads
// once more after a pause, recording whether Close of the rewrite had already
// returned by then.
type blockingSource struct {
	sent, closed  chan struct{}
	closeReturned atomic.Bool
	lateRead      atomic.Bool
	once          sync.Once
}

func (s *blockingSource) Read(p []byte) (int, error) {
	select {
	case <-s.sent:
	default:
		close(s.sent)
		return copy(p, []byte{0x1f, 0x8b}), nil
	}
	<-s.closed
	time.Sleep(50 * time.Millisecond)
	s.lateRead.Store(s.closeReturned.Load())
	return 0, io.ErrClosedPipe
}

func (s *blockingSource) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// The preparation reads what the upstream body counted once the rewrite is
// closed; the rewrite must not be reading the body by then.
func TestRewriteStream_CloseStopsReading(t *testing.T) {
	src := &blockingSource{sent: make(chan struct{}), closed: make(chan struct{})}
	rw := rewriteStream(src, "proxy.example.com", log.New(io.Discard, "", 0))
	<-src.sent
	rw.Close()
	src.closeReturned.Store(true)
	time.Sleep(100 * time.Millisecond)
	if src.lateRead.Load() {
		t.Fatal("the rewrite still read its source after Close returned")
	}
}

// unrewritableChart is a chart helm loads whose rewrite fails: archive/tar
// reads a regular file named with a trailing "/" but refuses to write one.
func unrewritableChart(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, f := range [][2]string{
		{"demo/Chart.yaml", chartWithDependency("demo", "https://top.example.com/charts")},
		{"demo/templates/cm.yaml", "kind: ConfigMap\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(f[1]))
	}
	_ = tw.Close()
	b := raw.Bytes()
	h := bytes.Index(b, []byte("demo/templates/cm.yaml"))
	b[h+len("demo/templates/cm.yaml")-1] = '/'
	copy(b[h+148:h+156], "        ")
	sum := 0
	for _, c := range b[h : h+512] {
		sum += int(c)
	}
	copy(b[h+148:h+156], fmt.Sprintf("%06o\x00 ", sum))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(b)
	_ = zw.Close()
	if _, err := rewriteChart(io.Discard, bytes.NewReader(gz.Bytes()), "proxy.example.com", log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("the rewrite succeeded; the test chart proves nothing")
	}
	return gz.Bytes()
}

// Before streaming, a chart whose dependencies could not be rewritten was
// served unchanged instead of failing.
func TestRewrite_FailureServesOriginal(t *testing.T) {
	f := newStreamFixture(t)
	f.m.config.RewriteDependencies = true
	f.m.config.ProxyHost = "proxy.example.com"
	tgz := unrewritableChart(t)
	f.serve.Store(tgz)
	d := f.layer(t)
	if d != digest.FromBytes(tgz) {
		t.Fatalf("layer %s, want the original archive %s", d, digest.FromBytes(tgz))
	}
	if got := readLayer(t, f, d); !bytes.Equal(got, tgz) {
		t.Fatal("layer GET did not return the original archive")
	}
}

// Only a failure of the rewrite itself falls back to the original archive; an
// archive that cannot be read is an upstream fault and is not fetched again.
func TestRewrite_ReadFailureIsNotRetriedUnrewritten(t *testing.T) {
	f := newStreamFixture(t)
	f.m.config.RewriteDependencies = true
	f.m.config.ProxyHost = "proxy.example.com"
	tgz := tarballOf(t, [][2]string{{"demo/Chart.yaml", chartWithDependency("demo", "https://top.example.com/charts")}})
	f.serve.Store(tgz[:len(tgz)/2])
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+f.repo+"/manifests/1.0.0", nil)
	if got := regStatus(t, f.m.Handle(rec, req)); got != http.StatusBadGateway {
		t.Errorf("status %d, want 502", got)
	}
	if n := f.downloads.Load(); n != 1 {
		t.Errorf("%d downloads of a truncated archive, want 1", n)
	}
}

// helm cleans entry paths, so demo/./Chart.yaml is the chart's own Chart.yaml
// and demo/charts/sub/./Chart.yaml is still a subchart's.
func TestRewrite_ChartYAMLPathIsNormalised(t *testing.T) {
	tgz := tarballOf(t, [][2]string{
		{"demo/charts/sub/./Chart.yaml", chartWithDependency("sub", "https://sub.example.com/charts")},
		{"demo/./Chart.yaml", chartWithDependency("demo", "https://top.example.com/charts")},
	})
	var out bytes.Buffer
	if _, err := rewriteChart(&out, bytes.NewReader(tgz), "proxy.example.com", log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	got := chartYAMLs(t, out.Bytes())
	if r := got["demo/./Chart.yaml"].Dependencies[0].Repository; r != "oci://proxy.example.com/top.example.com/charts" {
		t.Errorf("chart dependency repository = %s, not rewritten", r)
	}
	if r := got["demo/charts/sub/./Chart.yaml"].Dependencies[0].Repository; r != "https://sub.example.com/charts" {
		t.Errorf("subchart dependency repository = %s, want it unchanged", r)
	}
}

// A source whose archive cannot be rewritten fails before EOF; it must move to
// the back like a mismatching one, so the retry reaches the next source.
func TestRewrite_FailedSourceIsTriedLast(t *testing.T) {
	tgz := tarballOf(t, [][2]string{{"demo/Chart.yaml", chartWithDependency("demo", "https://top.example.com/charts")}})
	var rewritten bytes.Buffer
	if _, err := rewriteChart(&rewritten, bytes.NewReader(tgz), "proxy.example.com", log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	m, base, _ := layerUpstream(t, map[string][]byte{"/a.tgz": []byte("not a gzip archive"), "/b.tgz": tgz})
	d := holdLayer(m, "1.0.0", rewritten.Bytes(), false, base+"/a.tgz", base+"/b.tgz")
	m.mu.Lock()
	m.records[testRepo]["1.0.0"].layer.rewriteHost = "proxy.example.com"
	m.mu.Unlock()
	if _, err := readBlob(t, m, d); err == nil {
		t.Fatal("first read of the broken source succeeded")
	}
	if got, err := readBlob(t, m, d); err != nil || !bytes.Equal(got, rewritten.Bytes()) {
		t.Fatalf("retry was not served by the other source: %v", err)
	}
}

// A client that disconnects mid-layer says nothing about the source.
func TestStreamedLayer_ClientCancelKeepsSourceOrder(t *testing.T) {
	f := newStreamFixture(t)
	d := f.layer(t)
	f.m.mu.Lock()
	l := f.m.records[f.repo]["1.0.0"].layer
	first := l.urls[0]
	l.urls = append(l.urls, first+"?second")
	f.m.mu.Unlock()
	f.delay.Store(int64(300 * time.Millisecond))
	ctx, cancel := context.WithCancel(t.Context())
	b, _ := f.m.findBlob(f.repo, d)
	rc, err := f.m.openBlob(ctx, f.repo, b)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("read survived the cancelled request")
	}
	if b, _ := f.m.findBlob(f.repo, d); b.urls[0] != first {
		t.Errorf("source order %v after a client cancel, want %s first", b.urls, first)
	}
}

// readChartMetadata also splits entry names on "\", so demo\Chart.yaml is the
// chart's own Chart.yaml for the rewrite as well.
func TestRewrite_BackslashChartYAMLIsRewritten(t *testing.T) {
	tgz := tarballOf(t, [][2]string{{`demo\Chart.yaml`, chartWithDependency("demo", "https://top.example.com/charts")}})
	if _, err := readChartMetadata(bytes.NewReader(tgz)); err != nil {
		t.Fatalf("readChartMetadata rejected the chart: %v", err)
	}
	modified, err := rewriteChart(io.Discard, bytes.NewReader(tgz), "proxy.example.com", log.New(io.Discard, "", 0))
	if err != nil || !modified {
		t.Fatalf("rewriteChart: modified %v, %v; want the chart's Chart.yaml rewritten", modified, err)
	}
}

// A rewrite whose reader went away fails writing to the pipe; that is not a
// failure of the rewrite, so the preparation must not fall back to fetching
// the chart again. gzip writes its header on the first tar header, so the
// closed pipe surfaces from tw.WriteHeader.
func TestRewriteStream_ClosedReaderIsNotARewriteFailure(t *testing.T) {
	tgz := tarballOf(t, [][2]string{{"demo/Chart.yaml", chartWithDependency("demo", "https://top.example.com/charts")}})
	rw := rewriteStream(io.NopCloser(bytes.NewReader(tgz)), "proxy.example.com", log.New(io.Discard, "", 0))
	rw.Close()
	if rw.err == nil {
		t.Fatal("the rewrite did not fail writing to the closed pipe; the test proves nothing")
	}
	if rw.failed() {
		t.Fatalf("a closed pipe was classified as a rewrite failure: %v", rw.err)
	}
}
