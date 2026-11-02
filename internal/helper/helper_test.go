package helper

import "testing"

// Manifests, tags and blobs share one parser, so a blob is looked up under the
// key its manifest was stored with. A "v2" segment inside the repository path
// used to cut the manifest repository short.
func TestSplitPath(t *testing.T) {
	for _, tc := range []struct{ path, repo, kind, ref string }{
		{"/v2/charts.example.com/demo/manifests/1.0.0", "charts.example.com/demo", "manifests", "1.0.0"},
		{"/v2/h/v2/demo/blobs/sha256:ab", "h/v2/demo", "blobs", "sha256:ab"},
		{"/v2/h/demo/tags/list", "h/demo", "tags", "list"},
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
