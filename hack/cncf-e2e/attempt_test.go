package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// The first attempt gets a manifest, the retry a 500. The proxy-bug row must
// describe the retry, not carry the first attempt's digests next to its evidence.
func TestCheck_RetryResetsDigests(t *testing.T) {
	tgz := chartTgz(t, "demo", "1.0.0", "")
	var up *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - apiVersion: v2\n      name: demo\n      version: 1.0.0\n      urls: [%s/demo-1.0.0.tgz]\n", up.URL)
	})
	mux.HandleFunc("/demo-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) })
	up = httptest.NewTLSServer(mux)
	defer up.Close()

	const manifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":1},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size":1}]}`
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) > 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digestOf([]byte(manifest)))
		_, _ = w.Write([]byte(manifest))
	}))
	defer proxy.Close()

	pu, _ := url.Parse(proxy.URL)
	k := newChecker(pu, proxy.Client(), guardUpstream(up.Client(), true), "helm", t.TempDir(), 0, 0)
	p := Package{Name: "demo", Version: "1.0.0"}
	p.Repository.URL = up.URL
	r := k.check(job{pkg: p, version: p.Version})
	if r.Class != classProxyBug || r.Evidence == nil || r.Evidence.Status != http.StatusInternalServerError {
		t.Fatalf("class %s evidence %+v, want proxy-bug with the retry's 500", r.Class, r.Evidence)
	}
	if r.ManifestDigest != "" || r.LayerDigest != "" {
		t.Errorf("manifest %q layer %q survive from the first attempt", r.ManifestDigest, r.LayerDigest)
	}
}
