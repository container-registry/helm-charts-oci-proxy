// cncf-e2e proxies the Helm chart of every CNCF package listed on Artifact Hub
// through a running helm-charts-oci-proxy and checks each result against the
// upstream chart repository.
//
//	cncf-e2e -proxy http://127.0.0.1:19100 -diag http://127.0.0.1:19101 \
//	  -target docker:chartproxy-e2e -out /tmp/cncf-e2e -helm /path/to/helm3
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const userAgent = "helm-charts-oci-proxy-cncf-e2e"

// defaultRetryDelay outlasts the proxy's default INDEX_ERROR_CACHE_TTL of 30s
// (cmd/serve.go), so the retry does not just read back a cached failure.
const defaultRetryDelay = 35 * time.Second

func main() {
	proxy := flag.String("proxy", "http://127.0.0.1:19100", "proxy base URL")
	diag := flag.String("diag", "", "proxy DIAG_ADDR base URL, for Go memstats and counters (optional)")
	target := flag.String("target", "", "docker:<container> to sample cgroup memory of the proxy (optional)")
	out := flag.String("out", "cncf-e2e-out", "output directory for results.json and summary.md")
	discFile := flag.String("discovery", "", "discovery cache file (default <out>/discovery.json)")
	refresh := flag.Bool("refresh", false, "query Artifact Hub even if the discovery cache exists")
	deprecated := flag.Bool("deprecated", false, "include deprecated packages")
	ahBase := flag.String("artifacthub", "https://artifacthub.io", "Artifact Hub base URL")
	conc := flag.Int("concurrency", 8, "charts checked at once")
	perHost := flag.Int("per-host", 2, "charts checked at once per chart repository host")
	allVersions := flag.Bool("all-versions", false, "check every version in the index, not only the latest")
	helmBin := flag.String("helm", "helm", "helm 3 binary")
	only := flag.String("only", "", "regexp on <repository URL>/<chart>; check only matching charts")
	label := flag.String("label", "", "name of the build under test, for the summary")
	timeout := flag.Duration("timeout", 3*time.Minute, "timeout of each HTTP request and helm command")
	retryDelay := flag.Duration("retry-delay", defaultRetryDelay, "wait before repeating failed proxy checks; must exceed the proxy's INDEX_ERROR_CACHE_TTL")
	flag.Parse()
	if *conc < 1 || *perHost < 1 {
		log.Fatal("-concurrency and -per-host must be positive")
	}
	var onlyRe *regexp.Regexp
	if *only != "" {
		onlyRe = regexp.MustCompile(*only)
	}
	pu, err := url.Parse(strings.TrimSuffix(*proxy, "/"))
	if err != nil || pu.Host == "" || (pu.Scheme != "http" && pu.Scheme != "https") {
		log.Fatalf("-proxy %q: want http(s)://host:port", *proxy)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	if *discFile == "" {
		*discFile = filepath.Join(*out, "discovery.json")
	}
	if err := checkHelm3(*helmBin); err != nil {
		log.Fatal(err)
	}

	client := &http.Client{Timeout: *timeout}
	if err := ping(client, pu.String()+"/v2/"); err != nil {
		log.Fatalf("proxy not reachable: %v", err)
	}
	disc, cached, err := loadOrDiscover(*discFile, *refresh, client, strings.TrimSuffix(*ahBase, "/"), *deprecated)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%d CNCF Helm packages (%s, cached=%v)", len(disc.Packages), disc.Query, cached)

	work, err := os.MkdirTemp("", "cncf-e2e-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(work)
	k := newChecker(pu, client, *helmBin, work, *timeout, *retryDelay)

	jobs := buildJobs(k, disc.Packages, *allVersions, onlyRe)
	log.Printf("%d charts to check, concurrency %d, %d per host", len(jobs), *conc, *perHost)

	ctx, stopSampling := context.WithCancel(context.Background())
	mem := &memStats{}
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		mem.sample(ctx, *target, *diag)
	}()

	start := time.Now()
	results := runJobs(k, jobs, *conc, *perHost)
	wall := time.Since(start)
	stopSampling()
	<-sampled
	mem.finish(*target)
	counters := proxyCounters(client, *diag)

	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.RepoURL != b.RepoURL {
			return a.RepoURL < b.RepoURL
		}
		if a.Chart != b.Chart {
			return a.Chart < b.Chart
		}
		return a.Version < b.Version
	})
	rep := &Report{
		Label:       *label,
		Proxy:       pu.String(),
		StartedAt:   start.UTC(),
		WallSeconds: wall.Seconds(),
		Concurrency: *conc,
		PerHost:     *perHost,
		AllVersions: *allVersions,
		Discovery: DiscoverySummary{
			Query: disc.Query, FetchedAt: disc.FetchedAt, Packages: len(disc.Packages), Cached: cached,
		},
		Memory:   mem,
		Counters: counters,
		Results:  results,
	}
	if err := rep.write(*out); err != nil {
		log.Fatal(err)
	}
	log.Printf("done in %s: %v; results in %s", wall.Round(time.Second), rep.classCounts(), *out)
}

func checkHelm3(bin string) error {
	v, err := exec.Command(bin, "version", "--template", "{{.Version}}").Output()
	if err != nil {
		return fmt.Errorf("%s version: %w", bin, err)
	}
	if !strings.HasPrefix(string(v), "v3.") {
		return fmt.Errorf("%s is %s; use a helm 3 binary (-helm), helm 4 OCI output differs", bin, v)
	}
	return nil
}

func ping(c *http.Client, u string) error {
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return nil
}

type job struct {
	pkg     Package
	version string
}

func (j job) host() string {
	u, err := url.Parse(j.pkg.Repository.URL)
	if err != nil {
		return j.pkg.Repository.URL
	}
	return u.Host
}

func buildJobs(k *checker, pkgs []Package, allVersions bool, only *regexp.Regexp) []job {
	var jobs []job
	for _, p := range pkgs {
		if only != nil && !only.MatchString(strings.TrimSuffix(p.Repository.URL, "/")+"/"+p.Name) {
			continue
		}
		if !allVersions || !strings.HasPrefix(p.Repository.URL, "https://") {
			jobs = append(jobs, job{pkg: p, version: p.Version})
			continue
		}
		idx, err := k.idx.get(p.Repository.URL)
		if err != nil || len(idx.Entries[p.Name]) == 0 {
			// The check reports why.
			jobs = append(jobs, job{pkg: p, version: p.Version})
			continue
		}
		for _, cv := range idx.Entries[p.Name] {
			jobs = append(jobs, job{pkg: p, version: cv.Version})
		}
	}
	return interleaveByHost(jobs)
}

// interleaveByHost orders jobs round-robin across hosts so the per-host limit
// does not leave workers waiting on one host while others are idle.
func interleaveByHost(jobs []job) []job {
	byHost := map[string][]job{}
	var hosts []string
	for _, j := range jobs {
		h := j.host()
		if _, ok := byHost[h]; !ok {
			hosts = append(hosts, h)
		}
		byHost[h] = append(byHost[h], j)
	}
	out := make([]job, 0, len(jobs))
	for len(out) < len(jobs) {
		for _, h := range hosts {
			if q := byHost[h]; len(q) > 0 {
				out = append(out, q[0])
				byHost[h] = q[1:]
			}
		}
	}
	return out
}

func runJobs(k *checker, jobs []job, conc, perHost int) []Result {
	ch := make(chan job)
	var mu sync.Mutex
	hostSem := map[string]chan struct{}{}
	sem := func(h string) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		s, ok := hostSem[h]
		if !ok {
			s = make(chan struct{}, perHost)
			hostSem[h] = s
		}
		return s
	}
	results := make([]Result, 0, len(jobs))
	var wg sync.WaitGroup
	for range conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				s := sem(j.host())
				s <- struct{}{}
				r := k.check(j)
				<-s
				mu.Lock()
				results = append(results, r)
				n := len(results)
				mu.Unlock()
				log.Printf("[%d/%d] %-15s %s/%s %s %s", n, len(jobs), r.Class, strings.TrimSuffix(r.RepoURL, "/"), r.Chart, r.Version, r.Reason)
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return results
}
