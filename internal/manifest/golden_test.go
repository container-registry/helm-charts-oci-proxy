package manifest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Fixed archives, so the digests below depend only on the proxy's conversion.
const (
	goldenChart        = "H4sIAAAAAAAA/+yVTWvzMAzHe/an0Bd4EuXl6QM+PdDDYLDb2F0kauotfsF2SvvtR70XRmD0MNJt1L+LgpAUWfgv96xtudmRj8WR9LhaAkTEddsmi4hzi83f9ft38lc1rusV4CLdzJhCJL/CL/9rfrhfAjn1wD4oayTsa2FIs4TTpRD7N3dV1EUjeg6dVy4m140dezbQq4FDhK06xMmz2NlT8i5GF2RZ8oG0G7norC5TwWAn33GQAuDPp1Fl8J3QpEwkZdi/Rr+0dUuGBQAAa1KjhEcy/P9DAUHG2EinFlNeR5EH648S7jlEZQbx3dP+eaSZ72mcOCy2AM7pH6t2rv8G/2X9XwLPblQdBQlVVscVkvQfWbuRIoey0wtsgbP6r3Gm/6bF/P5fhCdlegkba7ZquCOXd0Amk8lcCc8BAAD//7gCARcAEAAA"
	goldenChartWithDep = "H4sIAAAAAAAA/+yUz47TMBDGe/ZT+AWwJ2npSj4h7QEJiRviPjjTdsD/5HGr3bdH9bJQItAeUCsQ+V0cjTxfPif+ZqKY7f0BazOPGMPqGgAAbDebvgLAfIXt+Pr7c68PI2yHlYaruJlxlIZ1BX/8rvnh/hGw8Eeqwjk5fRpVwkhOny+FOj2XBzOatZpIfOXSeultDhMlPfGepOkdP7RjJXXI5+ZDa0WctfSAsQQyPkfbBSUfqydxSutXv91lpXoVkVNDTlS/7X6y9Q4TKa21pogcnP6Mid5cCChMKTc8W+x9Hhvtc310+gNJ47RXExVKEyXP9JNypYmlS/849Z0BA71WqWTh1pWefftzZMRc2peGnwJdqIbsMcxUfym640DOWmPsU8sN/3//5icMR5KrDYCX8g/DZp7/Ndwt+b8FlUpgj+L0cMtrt/CX0PPfKJaAjcT6eIUp8GL+R5jlf72Bccn/LfjCaXL6Pqcd799jWWbAwsLCwn/C1wAAAP//oHdkEQAQAAA="
)

func goldenPrepare(t *testing.T, archive string, rewrite bool) (manifestDigest, configDigest string) {
	t.Helper()
	tgz, err := base64.StdEncoding.DecodeString(archive)
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - apiVersion: v2\n      name: demo\n      version: 1.2.3\n      created: \"2024-05-06T07:08:09Z\"\n      urls: [%s/demo-1.2.3.tgz]\n", srv.URL)
	})
	mux.HandleFunc("/demo-1.2.3.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) })
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	m := newTestManifests(t, Config{RewriteDependencies: rewrite, ProxyHost: "proxy.example.com"})
	m.httpClient = srv.Client()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/"+strings.TrimPrefix(srv.URL, "https://")+"/demo/manifests/1.2.3", nil)
	if err := m.Handle(rec, req); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man ocispec.Manifest
	if err := json.Unmarshal(rec.Body.Bytes(), &man); err != nil {
		t.Fatal(err)
	}
	return rec.Header().Get("Docker-Content-Digest"), man.Config.Digest.String()
}

// Clients key their caches by these digests; any change in how a manifest is
// built makes every chart look new to them.
func TestManifestDigestsAreStable(t *testing.T) {
	for _, tc := range []struct {
		name, archive    string
		rewrite          bool
		manifest, config string
	}{
		{"streamed", goldenChart, false,
			"sha256:67c3d97ff6fd14b3f8efdf1cdaf2e56b3131ed6e6e033b1c491b3d06f23c4104",
			"sha256:40d3a03cc9a7995dcf14dd08c484389561a7f129a7edcad12bb0e7b000557d05"},
		{"rewrite enabled, nothing to rewrite", goldenChart, true,
			"sha256:67c3d97ff6fd14b3f8efdf1cdaf2e56b3131ed6e6e033b1c491b3d06f23c4104",
			"sha256:40d3a03cc9a7995dcf14dd08c484389561a7f129a7edcad12bb0e7b000557d05"},
		{"streamed, dependency kept", goldenChartWithDep, false,
			"sha256:f3c76e29b5ccc75fc1ad21c8bab7d4b8cc5fda6833d4f3cbda08168fb9261805",
			"sha256:203439f991e2fe22efcd8d69beafffbddc84fe710ef2bb6ef430973b96388b16"},
		{"rewritten dependency", goldenChartWithDep, true,
			"sha256:c3ac7038adcc1dff26190c2aff1c4d1a475899d9e06956355cb0d50ae36e1227",
			"sha256:e730766dfa22a025996bee64a3f2813471d487044bb21acb338cfb6c7165b24d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, cd := goldenPrepare(t, tc.archive, tc.rewrite)
			if md != tc.manifest || cd != tc.config {
				t.Errorf("manifest %s config %s; want manifest %s config %s", md, cd, tc.manifest, tc.config)
			}
		})
	}
}
