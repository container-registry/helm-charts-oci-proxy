// replay sends the registry requests recorded in an ingress-nginx access log
// export (VictoriaLogs JSONL) to a proxy, at their original relative timing,
// and samples the proxy's memory while it runs.
//
//	replay -log ingress.jsonl -from 2026-11-01T07:59:16Z -to 2026-11-01T08:08:14Z \
//	  -proxy http://127.0.0.1:19000 -diag http://127.0.0.1:19001 \
//	  -target docker:chartproxy-diag -out run/
//
// The load is open loop: a request is sent when its time comes, whether or not
// earlier ones have finished, as independent production clients behave. A
// closed-loop replay lets a slow proxy throttle its own load and hides the
// overload that is being reproduced.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/hack/internal/accesslog"
)

func main() {
	logFile := flag.String("log", "", "VictoriaLogs JSONL export of ingress-nginx access lines")
	from := flag.String("from", "", "window start, RFC 3339 (e.g. 2026-11-01T07:59:16Z)")
	to := flag.String("to", "", "window end, RFC 3339")
	proxy := flag.String("proxy", "http://127.0.0.1:19000", "proxy base URL")
	diag := flag.String("diag", "http://127.0.0.1:19001", "proxy DIAG_ADDR base URL")
	target := flag.String("target", "", "PID, or docker:<container> to read cgroup memory")
	out := flag.String("out", "replay-out", "output directory")
	speed := flag.Float64("speed", 1, "time compression factor")
	maxConc := flag.Int("max-concurrency", 2048, "cap on in-flight requests; requests due while it is reached are dropped and counted")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request client timeout")
	flag.Parse()
	if *speed <= 0 || *maxConc <= 0 {
		log.Fatal("-speed and -max-concurrency must be positive")
	}

	winFrom, winTo, err := accesslog.ParseWindow(*from, *to)
	if err != nil {
		log.Fatal(err)
	}
	reqs, err := accesslog.Read(*logFile, winFrom, winTo)
	if err != nil {
		log.Fatal(err)
	}
	if len(reqs) == 0 {
		log.Fatal("no registry requests in window")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	span := reqs[len(reqs)-1].At.Sub(reqs[0].At)
	log.Printf("replaying %d requests spanning %s at speed %.1f (open loop, max %d in flight)", len(reqs), span, *speed, *maxConc)

	var inflight atomic.Int64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		sample(*diag, *target, *out, &inflight, stop)
	}()

	// The default transport keeps 2 idle connections per host; with hundreds of
	// requests in flight every one would dial anew and churn ephemeral ports.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = *maxConc
	tr.MaxIdleConnsPerHost = *maxConc
	client := &http.Client{Timeout: *timeout, Transport: tr}
	sem := make(chan struct{}, *maxConc)
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	dropped := 0
	start := time.Now()
	for _, r := range reqs {
		due := time.Duration(float64(r.At.Sub(reqs[0].At)) / *speed)
		if d := due - time.Since(start); d > 0 {
			time.Sleep(d)
		}
		// Blocking here would turn the open loop into backpressure exactly when
		// the proxy is overloaded.
		select {
		case sem <- struct{}{}:
		default:
			dropped++
			continue
		}
		wg.Add(1)
		inflight.Add(1)
		go func(r accesslog.Request) {
			defer func() { <-sem; inflight.Add(-1); wg.Done() }()
			code := fire(client, *proxy, r)
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	time.Sleep(2 * time.Second)
	close(stop)
	<-sampled

	keys := make([]int, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d:%d", k, codes[k]))
	}
	log.Printf("done in %s status={%s} dropped=%d (0 = client error or timeout, dropped = concurrency cap reached)",
		time.Since(start).Round(time.Second), strings.Join(parts, " "), dropped)
}

func fire(c *http.Client, base string, r accesslog.Request) int {
	req, err := http.NewRequest(r.Method, base+r.Path, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "harbor-registry-client")
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp, err := c.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

type vars struct {
	MemStats struct {
		HeapInuse uint64
		HeapSys   uint64
		NumGC     uint32
	} `json:"memstats"`
	BlobStoreBytes int64 `json:"blob_store_bytes"`
	BlobStoreBlobs int64 `json:"blob_store_blobs"`
	ChartsPrepared int64 `json:"charts_prepared"`
	ChartAttempts  int64 `json:"chart_prepare_attempts"`
	IndexParses    int64 `json:"index_parses"`
}

// sample writes a CSV row at most every 200ms. The docker exec and expvar
// fetch per row add latency, so rows are often further apart, and the cgroup
// and heap readings of one row are taken a few milliseconds apart. Heap and
// goroutine profiles are saved each time the in-use heap passes the last saved
// value by 50MiB, so heap-peak.pb.gz can be up to 50MiB below the true peak.
func sample(diag, target, out string, inflight *atomic.Int64, stop <-chan struct{}) {
	f, err := os.Create(filepath.Join(out, "mem.csv"))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, "t,mem_mib,heap_inuse_mib,heap_sys_mib,num_gc,blob_store_mib,blobs,charts_prepared,chart_prepare_attempts,index_parses,inflight")
	c := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	var peak uint64
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		t := time.Since(start).Seconds()
		var v vars
		if err := getJSON(c, diag+"/debug/vars", &v); err != nil {
			fmt.Fprintf(f, "%.1f,ERR %v\n", t, err)
			continue
		}
		mem, err := memMiB(target)
		if err != nil {
			fmt.Fprintf(f, "%.1f,ERR %v\n", t, err)
			continue
		}
		const mib = 1 << 20
		fmt.Fprintf(f, "%.1f,%.0f,%d,%d,%d,%d,%d,%d,%d,%d,%d\n", t, mem,
			v.MemStats.HeapInuse/mib, v.MemStats.HeapSys/mib, v.MemStats.NumGC,
			v.BlobStoreBytes/mib, v.BlobStoreBlobs, v.ChartsPrepared, v.ChartAttempts, v.IndexParses, inflight.Load())
		if v.MemStats.HeapInuse > peak+50*mib {
			peak = v.MemStats.HeapInuse
			save(c, diag+"/debug/pprof/heap", filepath.Join(out, "heap-peak.pb.gz"))
			save(c, diag+"/debug/pprof/goroutine?debug=1", filepath.Join(out, "goroutine-peak.txt"))
		}
	}
}

func getJSON(c *http.Client, url string, v any) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func save(c *http.Client, url, path string) {
	resp, err := c.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}

// memMiB reports what the OOM killer counts for a container: cgroup usage
// minus inactive page cache, which the kernel reclaims before killing.
func memMiB(target string) (float64, error) {
	if name, ok := strings.CutPrefix(target, "docker:"); ok {
		raw, err := exec.Command("docker", "exec", name, "cat", "/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.stat").Output()
		if err != nil {
			return 0, err
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
	if target == "" {
		return 0, nil
	}
	raw, err := exec.Command("ps", "-o", "rss=", "-p", target).Output()
	if err != nil {
		return 0, err
	}
	kib, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	return kib / 1024, err
}
