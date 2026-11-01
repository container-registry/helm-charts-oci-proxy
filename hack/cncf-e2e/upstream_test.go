package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The proxy refuses redirects to plain http and dials to non-public addresses.
// A direct fetch that ignored the same policy would reach charts the proxy
// cannot serve and report them as proxy-bug.
func TestCheck_UpstreamPolicyIsNotApplicable(t *testing.T) {
	tgz := chartTgz(t, "demo", "1.0.0", "")
	var up *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - apiVersion: v2\n      name: demo\n      version: 1.0.0\n      urls: [%s/demo-1.0.0.tgz]\n", up.URL)
	})
	mux.HandleFunc("/demo-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) })
	mux.HandleFunc("/redirect/index.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://chart-repo.invalid/index.yaml", http.StatusFound)
	})
	up = httptest.NewTLSServer(mux)
	defer up.Close()
	proxy := httptest.NewServer(http.NotFoundHandler())
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)

	for _, tc := range []struct {
		name, repo   string
		allowPrivate bool
	}{
		{"redirect to http", up.URL + "/redirect", true},
		{"private address", up.URL, false},
	} {
		k := newChecker(pu, up.Client(), guardUpstream(up.Client(), tc.allowPrivate), "helm", t.TempDir(), 0, 0)
		p := Package{Name: "demo", Version: "1.0.0"}
		p.Repository.URL = tc.repo
		if r := k.check(job{pkg: p, version: p.Version}); r.Class != classNA {
			t.Errorf("%s: class %s (%s), want %s", tc.name, r.Class, r.Reason, classNA)
		}
	}
}
