package helper

import "testing"

// Manifests, tags and blobs share one parser, so a blob is looked up under the
// key its manifest was stored with. Harbor's proxy cache puts its own path in
// front of /v2/; the repository starts after the last "v2" segment.
func TestSplitPath(t *testing.T) {
	for _, tc := range []struct{ path, repo, kind, ref string }{
		{"/v2/charts.example.com/demo/manifests/1.0.0", "charts.example.com/demo", "manifests", "1.0.0"},
		{"/v2/h/demo/tags/list", "h/demo", "tags", "list"},
		{"/charts.example.com/stable/v2/charts.example.com/stable/demo/manifests/1.0.0", "charts.example.com/stable/demo", "manifests", "1.0.0"},
		{"/h/v2/h/demo/blobs/sha256:ab", "h/demo", "blobs", "sha256:ab"},
		{"/a/b/v2/h/demo/tags/list", "h/demo", "tags", "list"},
		{"/v2/h/v2/demo/blobs/sha256:ab", "demo", "blobs", "sha256:ab"},
		{"/a/h/demo/manifests/1.0.0", "h/demo", "manifests", "1.0.0"},
	} {
		repo, kind, ref, ok := SplitPath(tc.path)
		if !ok || repo != tc.repo || kind != tc.kind || ref != tc.ref {
			t.Errorf("SplitPath(%q) = %q %q %q %v", tc.path, repo, kind, ref, ok)
		}
	}
	if _, _, _, ok := SplitPath("/v2/tags/list"); ok {
		t.Error("/v2/tags/list has no repository")
	}
}
