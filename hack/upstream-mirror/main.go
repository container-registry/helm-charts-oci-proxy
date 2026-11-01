// upstream-mirror records the chart repository content that a production
// traffic window touches and serves it back over HTTPS. Load replays then
// measure the proxy, not the network between the test machine and the
// upstream chart hosts.
//
//	upstream-mirror fetch -log ingress.jsonl -from 07:59:16 -to 08:08:14 -dir mirror
//	upstream-mirror serve -dir mirror -addr :443 -latency 20ms
//
// fetch resolves every requested chart version with the proxy's own index
// parser, so the mirror holds exactly the URLs the proxy will ask for. Upstream
// status codes and bodies are kept as-is, including 404 pages, because the
// proxy's behaviour on those is part of what is being measured.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/helper"
	"github.com/container-registry/helm-charts-oci-proxy/internal/manifest"
)

type entry struct {
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	File        string `json:"file"`
}

type store struct {
	dir     string
	mu      sync.Mutex
	entries map[string]entry
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		log.Fatal("usage: upstream-mirror fetch|serve [flags]")
	}
	switch os.Args[1] {
	case "fetch":
		fetch(os.Args[2:])
	case "serve":
		serve(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func fetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	logFile := fs.String("log", "", "VictoriaLogs JSONL export of ingress-nginx access lines")
	from := fs.String("from", "00:00:00", "window start, UTC HH:MM:SS")
	to := fs.String("to", "23:59:59", "window end, UTC HH:MM:SS")
	dir := fs.String("dir", "mirror", "mirror directory, reused across runs")
	workers := fs.Int("workers", 16, "parallel chart downloads")
	dry := fs.Bool("dry-run", false, "download indexes only and report chart counts")
	_ = fs.Parse(args)

	wanted, err := readRequests(*logFile, *from, *to)
	if err != nil {
		log.Fatal(err)
	}
	st := loadStore(*dir)
	client := &http.Client{Timeout: 120 * time.Second}

	var charts []string
	seen := map[string]bool{}
	for _, indexPath := range sortedKeys(wanted) {
		e, ok := st.download(client, "https://"+indexPath+"/index.yaml")
		if !ok || e.Status != http.StatusOK {
			continue
		}
		data, err := os.ReadFile(filepath.Join(st.dir, "files", e.File))
		if err != nil {
			log.Fatal(err)
		}
		// The proxy answers 404 for every chart of an unparsable index, so
		// nothing behind it is ever requested.
		idx, err := manifest.ParseIndex(data)
		if err != nil {
			log.Printf("index %s: %v", indexPath, err)
			continue
		}
		resolved := 0
		for ref := range wanted[indexPath] {
			cv, err := idx.Get(ref.chart, ref.version)
			if err != nil || len(cv.URLs) == 0 {
				continue
			}
			u, err := url.Parse(cv.URLs[0])
			if err == nil && !u.IsAbs() {
				u, err = url.Parse(fmt.Sprintf("https://%s/%s", indexPath, cv.URLs[0]))
			}
			if err != nil || u.Scheme != "https" {
				continue
			}
			resolved++
			if k := key(u); !seen[k] {
				seen[k] = true
				charts = append(charts, u.String())
			}
		}
		log.Printf("index %s: %d refs requested, %d resolve to https charts", indexPath, len(wanted[indexPath]), resolved)
	}
	st.save()
	log.Printf("%d index hosts, %d distinct chart URLs", len(wanted), len(charts))
	if *dry {
		return
	}

	jobs := make(chan string)
	var wg sync.WaitGroup
	var done atomic.Int64
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				st.download(client, u)
				if n := done.Add(1); n%500 == 0 {
					log.Printf("charts %d/%d", n, len(charts))
					st.save()
				}
			}
		}()
	}
	for _, u := range charts {
		jobs <- u
	}
	close(jobs)
	wg.Wait()
	st.save()

	hosts := st.hosts()
	if err := os.WriteFile(filepath.Join(st.dir, "hosts.txt"), []byte(strings.Join(hosts, "\n")+"\n"), 0o644); err != nil {
		log.Fatal(err)
	}
	if err := writeCerts(st.dir, hosts); err != nil {
		log.Fatal(err)
	}
	log.Printf("mirror ready: %d URLs, %d hosts", len(st.entries), len(hosts))
}

type chartRef struct{ chart, version string }

var accessLine = regexp.MustCompile(`^\S+ - \S+ \[[^\]]+\] "(?:GET|HEAD) (\S+) `)

// readRequests returns, per index path, the chart versions the proxy would
// look up for the window's manifest requests. Tag lists come from the index
// alone and need no chart downloads.
func readRequests(path, from, to string) (map[string]map[chartRef]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]map[chartRef]bool{}
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
		if m == nil {
			continue
		}
		p, _, _ := strings.Cut(m[1], "?")
		i := strings.LastIndex(p, "/v2/")
		if i < 0 {
			continue
		}
		p = p[i+len("/v2/"):]
		j := strings.LastIndex(p, "/manifests/")
		if j < 0 {
			if strings.HasSuffix(p, "/tags/list") {
				if repo := strings.TrimSuffix(p, "/tags/list"); strings.Contains(repo, "/") {
					indexPath := repo[:strings.LastIndex(repo, "/")]
					if out[indexPath] == nil {
						out[indexPath] = map[chartRef]bool{}
					}
				}
			}
			continue
		}
		repo, ref := p[:j], p[j+len("/manifests/"):]
		k := strings.LastIndex(repo, "/")
		if k < 0 || ref == "" || strings.HasPrefix(ref, "sha256:") {
			continue
		}
		// Same normalisation as Manifests.Handle followed by prepareChart.
		version := "v" + helper.TagToVersion(strings.TrimPrefix(ref, "v"))
		indexPath := repo[:k]
		if out[indexPath] == nil {
			out[indexPath] = map[chartRef]bool{}
		}
		out[indexPath][chartRef{repo[k+1:], version}] = true
	}
	return out, sc.Err()
}

func key(u *url.URL) string {
	return "https://" + u.Hostname() + u.RequestURI()
}

func loadStore(dir string) *store {
	st := &store{dir: dir, entries: map[string]entry{}}
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		log.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "entries.json")); err == nil {
		if err := json.Unmarshal(data, &st.entries); err != nil {
			log.Fatal(err)
		}
	}
	return st
}

func (s *store) save() {
	s.mu.Lock()
	data, err := json.Marshal(s.entries)
	s.mu.Unlock()
	if err != nil {
		log.Fatal(err)
	}
	tmp := filepath.Join(s.dir, "entries.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "entries.json")); err != nil {
		log.Fatal(err)
	}
}

// download records the upstream response for rawURL unless already present.
// Network errors are not recorded, so a later run retries them.
func (s *store) download(c *http.Client, rawURL string) (entry, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return entry{}, false
	}
	k := key(u)
	s.mu.Lock()
	e, ok := s.entries[k]
	s.mu.Unlock()
	if ok {
		return e, true
	}
	resp, err := c.Get(rawURL)
	if err != nil {
		log.Printf("fetch %s: %v", rawURL, err)
		return entry{}, false
	}
	defer resp.Body.Close()
	sum := sha256.Sum256([]byte(k))
	name := hex.EncodeToString(sum[:])
	f, err := os.Create(filepath.Join(s.dir, "files", name))
	if err != nil {
		log.Fatal(err)
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		log.Printf("fetch %s: %v", rawURL, err)
		return entry{}, false
	}
	e = entry{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), File: name}
	s.mu.Lock()
	s.entries[k] = e
	s.mu.Unlock()
	return e, true
}

func (s *store) hosts() []string {
	set := map[string]bool{}
	for k := range s.entries {
		if u, err := url.Parse(k); err == nil {
			set[u.Hostname()] = true
		}
	}
	return sortedKeys(set)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeCerts(dir string, hosts []string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "upstream-mirror CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "upstream-mirror"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"ca.pem":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", "mirror", "mirror directory written by fetch")
	addr := fs.String("addr", ":443", "listen address")
	latency := fs.Duration("latency", 0, "delay added before each response, to model upstream round trips")
	_ = fs.Parse(args)

	st := loadStore(*dir)
	var hits, misses atomic.Int64
	go func() {
		for range time.Tick(10 * time.Second) {
			log.Printf("served hits=%d misses=%d", hits.Load(), misses.Load())
		}
	}()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *latency > 0 {
			time.Sleep(*latency)
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		k := "https://" + host + r.URL.RequestURI()
		e, ok := st.entries[k]
		if !ok {
			misses.Add(1)
			log.Printf("miss %s", k)
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		f, err := os.Open(filepath.Join(st.dir, "files", e.File))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if e.ContentType != "" {
			w.Header().Set("Content-Type", e.ContentType)
		}
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		w.WriteHeader(e.Status)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, f)
		}
	})
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("serving %d URLs on %s (latency %s)", len(st.entries), *addr, *latency)
	log.Fatal(srv.ListenAndServeTLS(filepath.Join(st.dir, "tls.crt"), filepath.Join(st.dir, "tls.key")))
}
