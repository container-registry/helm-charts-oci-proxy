package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func chartTgz(t *testing.T, name, version, extra string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	meta := fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\ndescription: %q\n", name, version, extra)
	if err := tw.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0o644, Size: int64(len(meta))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(meta)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A version republished at a new URL during the run, with the old tarball still
// reachable, is an upstream change. Rechecking through the run-wide index cache
// would download the old URL again and blame the proxy.
func TestCheck_RecheckSeesRepublishedVersion(t *testing.T) {
	oldTgz, newTgz := chartTgz(t, "demo", "1.0.0", "old"), chartTgz(t, "demo", "1.0.0", "new")
	var indexReads atomic.Int32
	var up *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		file := "demo-1.0.0.tgz"
		if indexReads.Add(1) > 1 {
			file = "republished/demo-1.0.0.tgz"
		}
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - apiVersion: v2\n      name: demo\n      version: 1.0.0\n      urls: [%s/%s]\n", up.URL, file)
	})
	mux.HandleFunc("/demo-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(oldTgz) })
	mux.HandleFunc("/republished/demo-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(newTgz) })
	up = httptest.NewTLSServer(mux)
	defer up.Close()
	proxy := httptest.NewServer(http.NotFoundHandler())
	defer proxy.Close()

	pu, _ := url.Parse(proxy.URL)
	k := newChecker(pu, up.Client(), guardUpstream(up.Client(), true), "helm", t.TempDir(), 0, 0)
	p := Package{Name: "demo", Version: "1.0.0"}
	p.Repository.URL = up.URL
	r := k.check(job{pkg: p, version: p.Version})
	if r.Class != classUpstream || !strings.Contains(r.Reason, "changed") {
		t.Errorf("class %s (%s), want %s for a republished version", r.Class, r.Reason, classUpstream)
	}
}
