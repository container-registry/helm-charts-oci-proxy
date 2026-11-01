package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DiscoverySummary struct {
	Query     string    `json:"query"`
	FetchedAt time.Time `json:"fetched_at"`
	Packages  int       `json:"packages"`
	Cached    bool      `json:"cached"`
}

type Report struct {
	Label       string           `json:"label,omitempty"`
	Proxy       string           `json:"proxy"`
	StartedAt   time.Time        `json:"started_at"`
	WallSeconds float64          `json:"wall_seconds"`
	Concurrency int              `json:"concurrency"`
	PerHost     int              `json:"per_host"`
	AllVersions bool             `json:"all_versions"`
	Discovery   DiscoverySummary `json:"discovery"`
	Memory      *memStats        `json:"memory"`
	Counters    map[string]any   `json:"proxy_counters,omitempty"`
	Results     []Result         `json:"results"`
}

// memStats tracks the proxy's memory during the run. Cgroup working set is
// what the OOM killer acts on; Go heap figures come from expvar memstats.
type memStats struct {
	mu                sync.Mutex
	Samples           int      `json:"samples"`
	PeakWorkingSetMiB float64  `json:"peak_working_set_mib,omitempty"`
	CgroupPeakMiB     float64  `json:"cgroup_memory_peak_mib,omitempty"`
	PeakHeapInuseMiB  float64  `json:"peak_heap_inuse_mib,omitempty"`
	PeakGoSysMiB      float64  `json:"peak_go_sys_mib,omitempty"`
	OOMKilled         *bool    `json:"oom_killed,omitempty"`
	Errors            []string `json:"errors,omitempty"`
}

func (s *memStats) sample(ctx context.Context, target, diag string) {
	if target == "" && diag == "" {
		return
	}
	c := &http.Client{Timeout: 5 * time.Second}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		s.once(c, target, diag)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *memStats) once(c *http.Client, target, diag string) {
	var ws float64
	var wsErr, heapErr error
	var heap, sys float64
	if name, ok := strings.CutPrefix(target, "docker:"); ok {
		ws, wsErr = workingSetMiB(name)
	}
	if diag != "" {
		heap, sys, heapErr = goMem(c, diag)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Samples++
	s.PeakWorkingSetMiB = max(s.PeakWorkingSetMiB, ws)
	s.PeakHeapInuseMiB = max(s.PeakHeapInuseMiB, heap)
	s.PeakGoSysMiB = max(s.PeakGoSysMiB, sys)
	for _, err := range []error{wsErr, heapErr} {
		if err != nil && len(s.Errors) < 10 {
			s.Errors = append(s.Errors, err.Error())
		}
	}
}

// finish reads the kernel's own high-water mark, which also covers spikes
// shorter than the sampling interval.
func (s *memStats) finish(target string) {
	name, ok := strings.CutPrefix(target, "docker:")
	if !ok {
		return
	}
	if raw, err := exec.Command("docker", "exec", name, "cat", "/sys/fs/cgroup/memory.peak").Output(); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64); err == nil {
			s.CgroupPeakMiB = v / (1 << 20)
		}
	} else {
		s.Errors = append(s.Errors, "memory.peak: "+err.Error())
	}
	if raw, err := exec.Command("docker", "inspect", name, "--format", "{{.State.OOMKilled}}").Output(); err == nil {
		b := strings.TrimSpace(string(raw)) == "true"
		s.OOMKilled = &b
	}
}

// workingSetMiB is cgroup usage minus inactive page cache, which the kernel
// reclaims before it kills.
func workingSetMiB(container string) (float64, error) {
	raw, err := exec.Command("docker", "exec", container, "cat", "/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.stat").Output()
	if err != nil {
		return 0, fmt.Errorf("docker exec %s: %w", container, err)
	}
	lines := strings.Split(string(raw), "\n")
	cur, err := strconv.ParseFloat(strings.TrimSpace(lines[0]), 64)
	if err != nil {
		return 0, err
	}
	for _, l := range lines[1:] {
		if v, ok := strings.CutPrefix(l, "inactive_file "); ok {
			inactive, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
			cur -= inactive
			break
		}
	}
	return cur / (1 << 20), nil
}

func goMem(c *http.Client, diag string) (heap, sys float64, err error) {
	var v struct {
		Memstats struct {
			HeapInuse float64 `json:"HeapInuse"`
			Sys       float64 `json:"Sys"`
		} `json:"memstats"`
	}
	if err := getJSON(c, strings.TrimSuffix(diag, "/")+"/debug/vars", &v); err != nil {
		return 0, 0, err
	}
	return v.Memstats.HeapInuse / (1 << 20), v.Memstats.Sys / (1 << 20), nil
}

func getJSON(c *http.Client, u string, v any) error {
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// proxyCounters keeps the proxy's own expvar counters, without memstats and cmdline.
func proxyCounters(c *http.Client, diag string) map[string]any {
	if diag == "" {
		return nil
	}
	var all map[string]any
	if err := getJSON(c, strings.TrimSuffix(diag, "/")+"/debug/vars", &all); err != nil {
		return map[string]any{"error": err.Error()}
	}
	delete(all, "memstats")
	delete(all, "cmdline")
	return all
}

var classOrder = []string{classOK, classProxyBug, classUpstream, classNA}

func (r *Report) classCounts() map[string]int {
	m := map[string]int{}
	for _, res := range r.Results {
		m[res.Class]++
	}
	return m
}

func (r *Report) write(dir string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), raw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(r.markdown()), 0o644)
}

func (r *Report) markdown() string {
	var b strings.Builder
	title := "CNCF Helm charts through helm-charts-oci-proxy"
	if r.Label != "" {
		title += ": " + r.Label
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "- Proxy: `%s`, started %s, wall time %s\n", r.Proxy, r.StartedAt.Format(time.RFC3339), time.Duration(r.WallSeconds*float64(time.Second)).Round(time.Second))
	fmt.Fprintf(&b, "- Discovery: Artifact Hub `%s`, %d packages, fetched %s (from cache: %v)\n", r.Discovery.Query, r.Discovery.Packages, r.Discovery.FetchedAt.Format(time.RFC3339), r.Discovery.Cached)
	versions := "latest version per package"
	if r.AllVersions {
		versions = "every version in the index"
	}
	fmt.Fprintf(&b, "- Checked: %d charts (%s), concurrency %d, %d per repository host\n", len(r.Results), versions, r.Concurrency, r.PerHost)
	m := r.Memory
	fmt.Fprintf(&b, "- Proxy memory: peak working set %.1f MiB (sampled every 500ms, %d samples), cgroup memory.peak %.1f MiB, peak Go heap in use %.1f MiB, peak Go Sys %.1f MiB",
		m.PeakWorkingSetMiB, m.Samples, m.CgroupPeakMiB, m.PeakHeapInuseMiB, m.PeakGoSysMiB)
	if m.OOMKilled != nil {
		fmt.Fprintf(&b, ", OOM killed: %v", *m.OOMKilled)
	}
	b.WriteString("\n\n| Class | Count |\n|---|---|\n")
	counts := r.classCounts()
	for _, c := range classOrder {
		fmt.Fprintf(&b, "| %s | %d |\n", c, counts[c])
	}
	retried := 0
	for _, res := range r.Results {
		if res.Class == classOK && res.FirstFailure != "" {
			retried++
		}
	}
	if retried > 0 {
		fmt.Fprintf(&b, "\n%d ok results passed only on the second attempt; their first failure is in results.json (`first_failure`).\n", retried)
	}

	var noted []Result
	for _, res := range r.Results {
		if res.Class == classOK && res.Reason != "" {
			noted = append(noted, res)
		}
	}
	if len(noted) > 0 {
		b.WriteString("\n## Ok with notes\n\n| Repository | Chart | Version | Note |\n|---|---|---|---|\n")
		for _, res := range noted {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", res.RepoURL, res.Chart, res.Version, cell(res.Reason))
		}
	}

	var bad []Result
	for _, res := range r.Results {
		if res.Class != classOK {
			bad = append(bad, res)
		}
	}
	rank := map[string]int{}
	for i, c := range classOrder {
		rank[c] = i
	}
	sort.SliceStable(bad, func(i, j int) bool { return rank[bad[i].Class] < rank[bad[j].Class] })
	if len(bad) > 0 {
		b.WriteString("\n## Not ok\n\n| Class | Repository | Chart | Version | Reason |\n|---|---|---|---|---|\n")
		for _, res := range bad {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", res.Class, res.RepoURL, res.Chart, res.Version, cell(res.Reason))
		}
	}
	for _, res := range bad {
		if res.Class != classProxyBug || res.Evidence == nil {
			continue
		}
		e := res.Evidence
		fmt.Fprintf(&b, "\n### proxy-bug: %s/%s %s\n\n", strings.TrimSuffix(res.RepoURL, "/"), res.Chart, res.Version)
		fmt.Fprintf(&b, "- Step: %s\n- Request: `%s`\n- Status: %d, Content-Type: `%s`\n- Detail: %s\n", e.Step, e.Request, e.Status, e.ContentType, e.Detail)
		fmt.Fprintf(&b, "- Upstream: `%s` %s (%d bytes), index digest `%s`\n", res.DownloadURL, res.UpstreamDigest, res.UpstreamSize, res.IndexDigest)
		if res.ManifestDigest != "" {
			fmt.Fprintf(&b, "- Manifest %s, layer %s\n", res.ManifestDigest, res.LayerDigest)
		}
		if res.FirstFailure != "" {
			fmt.Fprintf(&b, "- First attempt: %s\n", cell(res.FirstFailure))
		}
		if e.Body != "" {
			fmt.Fprintf(&b, "\n```\n%s\n```\n", strings.TrimSpace(e.Body))
		}
	}
	return b.String()
}

func cell(s string) string {
	return strings.ReplaceAll(oneLine(s, 400), "|", "\\|")
}
