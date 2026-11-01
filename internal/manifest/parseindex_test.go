package manifest

import (
	"testing"
)

// ParseIndex must drop the same versions helm's repo.loadIndex drops. Before
// the fix the filtered slice was never written back, so an invalid version in
// the middle duplicated its neighbour, an invalid last version stayed, and an
// entry without metadata panicked.
func TestParseIndex_DropsInvalidVersionsLikeHelm(t *testing.T) {
	idx, err := ParseIndex([]byte(`apiVersion: v1
entries:
  demo:
    - name: demo
      version: 1.0.0
      urls: [demo-1.0.0.tgz]
    - name: demo
      version: not-semver
      urls: [demo-bad.tgz]
    - name: demo
      version: 2.0.0
      urls: [demo-2.0.0.tgz]
    - name: demo
      version: also-bad
      urls: [demo-bad2.tgz]
    -
    - urls: [no-metadata.tgz]
`))
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	var got []string
	for _, cv := range idx.Entries["demo"] {
		got = append(got, cv.Version)
	}
	if len(got) != 2 || got[0] != "2.0.0" || got[1] != "1.0.0" {
		t.Errorf("versions = %v, want [2.0.0 1.0.0]", got)
	}
}
