// replay sends the registry requests recorded in an ingress-nginx access log
// export (VictoriaLogs JSONL) to a proxy, at their original relative timing,
// and samples the proxy's memory while it runs.
//
//	replay -log ingress.jsonl -from 07:59:16 -to 08:08:14 \
//	  -proxy http://127.0.0.1:19000 -diag http://127.0.0.1:19001 \
//	  -target docker:chartproxy-diag -out run/
//
// The load is open loop: a request is sent when its time comes, whether or not
// earlier ones have finished, as independent production clients behave. A
// closed-loop replay lets a slow proxy throttle its own load and hides the
// overload that is being reproduced.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	accessLine = regexp.MustCompile(`^\S+ - \S+ \[[^\]]+\] "(GET|HEAD) (\S+) `)
	registryOp = regexp.MustCompile(`/(manifests|tags/list|blobs)/?`)
)

type request struct {
	at     time.Time
	method string
	path   string
}

func main() {
	logFile := flag.String("log", "", "VictoriaLogs JSONL export of ingress-nginx access lines")
	from := flag.String("from", "00:00:00", "window start, UTC HH:MM:SS")
	to := flag.String("to", "23:59:59", "window end, UTC HH:MM:SS")
	proxy := flag.String("proxy", "http://127.0.0.1:19000", "proxy base URL")
	diag := flag.String("diag", "http://127.0.0.1:19001", "proxy DIAG_ADDR base URL")
	target := flag.String("target", "", "PID, or docker:<container> to read cgroup memory")
	out := flag.String("out", "replay-out", "output directory")
	speed := flag.Float64("speed", 1, "time compression factor")
	maxConc := flag.Int("max-concurrency", 512, "cap on in-flight requests")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request client timeout")
	flag.Parse()

	reqs, err := load(*logFile, *from, *to)
	if err != nil {
		log.Fatal(err)
	}
	if len(reqs) == 0 {
		log.Fatal("no registry requests in window")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	span := reqs[len(reqs)-1].at.Sub(reqs[0].at)
	log.Printf("replaying %d requests spanning %s at speed %.1f (open loop, max %d in flight)", len(reqs), span, *speed, *maxConc)

	var inflight atomic.Int64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		sample(*diag, *target, *out, &inflight, stop)
	}()

	client := &http.Client{Timeout: *timeout}
	sem := make(chan struct{}, *maxConc)
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	start := time.Now()
	for _, r := range reqs {
		due := time.Duration(float64(r.at.Sub(reqs[0].at)) / *speed)
		if d := due - time.Since(start); d > 0 {
			time.Sleep(d)
		}
		sem <- struct{}{}
		wg.Add(1)
		inflight.Add(1)
		go func(r request) {
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
	log.Printf("done in %s status={%s} (0 = client error or timeout)", time.Since(start).Round(time.Second), strings.Join(parts, " "))
}

func load(path, from, to string) ([]request, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []request
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec struct {
			Time string `json:"_time"`
			Msg  string `json:"_msg"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || len(rec.Time) < 19 {
			continue
		}
		if hms := rec.Time[11:19]; hms < from || hms > to {
			continue
		}
		m := accessLine.FindStringSubmatch(rec.Msg)
		if m == nil || !registryOp.MatchString(m[2]) {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, rec.Time)
		if err != nil {
			continue
		}
		out = append(out, request{at: at, method: m[1], path: m[2]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out, sc.Err()
}

func fire(c *http.Client, base string, r request) int {
	req, err := http.NewRequest(r.method, base+r.path, nil)
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
	IndexParses    int64 `json:"index_parses"`
}

// sample writes one CSV row per 200ms and saves heap and goroutine profiles
// each time the in-use heap passes the previous peak by 50MiB, so the last
// profile written before an OOM kill is the closest available to the kill.
func sample(diag, target, out string, inflight *atomic.Int64, stop <-chan struct{}) {
	f, err := os.Create(filepath.Join(out, "mem.csv"))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, "t,mem_mib,heap_inuse_mib,heap_sys_mib,num_gc,blob_store_mib,blobs,charts_prepared,index_parses,inflight")
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
		fmt.Fprintf(f, "%.1f,%.0f,%d,%d,%d,%d,%d,%d,%d,%d\n", t, mem,
			v.MemStats.HeapInuse/mib, v.MemStats.HeapSys/mib, v.MemStats.NumGC,
			v.BlobStoreBytes/mib, v.BlobStoreBlobs, v.ChartsPrepared, v.IndexParses, inflight.Load())
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
