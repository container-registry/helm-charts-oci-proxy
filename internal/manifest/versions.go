package manifest

import (
	"strings"

	"github.com/container-registry/helm-charts-oci-proxy/internal/helper"
	"helm.sh/helm/v3/pkg/repo"
)

// ResolveVersion finds the index entry a manifest target names. An entry whose
// version equals the target wins; without one, the version with the leading "v"
// added or removed; only then is the target a semver constraint. Repositories
// such as charts.rook.io publish 1.21.1 and v1.21.1 as different tarballs.
func ResolveVersion(index *repo.IndexFile, chart, target string) (*repo.ChartVersion, error) {
	if target != "" {
		for _, want := range []string{target, toggleV(target)} {
			for _, cv := range index.Entries[chart] {
				if cv.Version == want {
					return cv, nil
				}
			}
		}
	}
	return index.Get(chart, target)
}

func toggleV(s string) string {
	if t, ok := strings.CutPrefix(s, "v"); ok {
		return t
	}
	return "v" + s
}

// versionTags turns the listed versions into OCI tags. The leading "v" is
// dropped, because helm 3 only considers strict semver tags when it resolves
// the latest version or a constraint, except where known also holds the version
// without it: there each chart keeps a tag of its own.
func versionTags(known, listed []string) []string {
	have := make(map[string]bool, len(known))
	for _, v := range known {
		have[v] = true
	}
	seen := make(map[string]bool, len(listed))
	tags := make([]string, 0, len(listed))
	for _, v := range listed {
		if bare, ok := strings.CutPrefix(v, "v"); ok && !have[bare] {
			v = bare
		}
		t := helper.VersionToTag(v)
		if !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	return tags
}

// writeAlias stores the manifest cached for version also under tag. copyMu
// keeps cleanup from deleting its blobs between the read and the write.
func (m *Manifests) writeAlias(repoKey, version, tag string) bool {
	m.copyMu.RLock()
	defer m.copyMu.RUnlock()
	ma, err := m.Read(repoKey, version)
	return err == nil && m.Write(repoKey, tag, ma) == nil
}
