package manifest

import (
	"expvar"
	"slices"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/opencontainers/go-digest"
)

// record is everything kept for one chart version: its manifest, the config
// blob and where the layer comes from. It is stored under the version and the
// manifest digest. A failed preparation is a record with err set, so a sweep
// over a missing version reaches the upstream once per INDEX_ERROR_CACHE_TTL.
type record struct {
	manifest []byte
	digest   string
	config   *blob
	layer    *blob
	err      *errors.RegError
	// A manifest is served until the first cleanup after expires; a failure
	// is ignored from expires on.
	expires time.Time
}

type blob struct {
	digest digest.Digest
	size   int64
	data   []byte // the config
	// Versions of a repository that publish the same tarball share one layer,
	// so every URL that served it is tried. Guarded by Manifests.mu.
	urls []string
	// The layer is the upstream tarball with its dependencies pointed here.
	rewriteHost string
}

// maxFailures bounds the keys that hold a failure. A failure that does not fit
// is not cached; a manifest always is, because its blobs are served from it.
const maxFailures = 16384

// expireInterval spaces the scans insert runs while failures are at the bound,
// so a sweep of missing versions does not scan every record on each insert.
const expireInterval = time.Second

var recordExpireScans = expvar.NewInt("record_expire_scans")

func (m *Manifests) lookup(repo, ref string) *record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.records[repo][ref]
	if r != nil && r.err != nil && time.Now().After(r.expires) {
		return nil
	}
	return r
}

func (m *Manifests) insert(repo string, r *record, refs ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.err != nil && m.failures+len(refs) > maxFailures {
		if now := time.Now(); now.After(m.nextExpire) {
			m.nextExpire = now.Add(expireInterval)
			m.expire(now)
		}
		if m.failures+len(refs) > maxFailures {
			return
		}
	}
	entries := m.records[repo]
	if entries == nil {
		entries = map[string]*record{}
		m.records[repo] = entries
	}
	if r.layer != nil {
		for _, o := range entries {
			if o.layer != nil && o.layer.digest == r.layer.digest && o.layer.rewriteHost == r.layer.rewriteHost {
				for _, u := range r.layer.urls {
					if !slices.Contains(o.layer.urls, u) {
						o.layer.urls = append(o.layer.urls, u)
					}
				}
				r.layer = o.layer
				break
			}
		}
	}
	for _, ref := range refs {
		old := entries[ref]
		// A concurrent preparation can store the chart under a digest that
		// another one just failed to resolve; the failure must not hide it.
		if r.err != nil && old != nil && old.err == nil {
			continue
		}
		if old != nil && old.err != nil {
			m.failures--
		}
		if r.err != nil {
			m.failures++
		}
		entries[ref] = r
	}
}

// fail remembers a definitive failure to prepare ref.
func (m *Manifests) fail(repo, ref string, err *errors.RegError) {
	if m.config.IndexErrorCacheTTL <= 0 {
		return
	}
	m.insert(repo, &record{err: err, expires: time.Now().Add(m.config.IndexErrorCacheTTL)}, ref)
}

// expire drops the records that expired before now. m.mu must be held.
func (m *Manifests) expire(now time.Time) {
	recordExpireScans.Add(1)
	for repo, entries := range m.records {
		for ref, r := range entries {
			if now.After(r.expires) {
				delete(entries, ref)
				if r.err != nil {
					m.failures--
				}
			}
		}
		if len(entries) == 0 {
			delete(m.records, repo)
		}
	}
}

func (m *Manifests) cleanup() {
	if m.config.Debug {
		m.log.Println("cleanup cycle")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(time.Now())
}

// names lists the references of repo that have a manifest.
func (m *Manifests) names(repo string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for ref, r := range m.records[repo] {
		if r.err == nil {
			out = append(out, ref)
		}
	}
	return out
}

func hasManifest(entries map[string]*record) bool {
	for _, r := range entries {
		if r.err == nil {
			return true
		}
	}
	return false
}

// findBlob looks d up among the blobs of the manifests held for repo.
func (m *Manifests) findBlob(repo string, d digest.Digest) (blob, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.records[repo] {
		for _, b := range []*blob{r.config, r.layer} {
			if b != nil && b.digest == d {
				c := *b
				c.urls = slices.Clone(b.urls)
				return c, true
			}
		}
	}
	return blob{}, false
}

// demote moves url to the end of the sources of layer d in repo.
func (m *Manifests) demote(repo string, d digest.Digest, url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.records[repo] {
		if r.layer != nil && r.layer.digest == d {
			if i := slices.Index(r.layer.urls, url); i >= 0 {
				r.layer.urls = append(slices.Delete(r.layer.urls, i, i+1), url)
			}
			return
		}
	}
}
