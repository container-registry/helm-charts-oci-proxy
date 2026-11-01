package manifest

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
)

// prepareTimeout bounds one shared chart preparation. The work is detached from
// the first caller's request, because every concurrent caller of the same
// version waits for it and the result is cached for all of them.
const prepareTimeout = 2 * time.Minute

const defaultPrepareConcurrency = 4

func (m *Manifests) manifest(ctx context.Context, repo, target string) (Manifest, *errors.RegError) {
	if ma, err := m.Read(repo, target); err == nil {
		return ma, nil
	}
	if err := m.prepare(ctx, repo, target); err != nil {
		return Manifest{}, err
	}
	if ma, err := m.Read(repo, target); err == nil {
		return ma, nil
	}
	return Manifest{}, resultNotFound(repo, target)
}

// prepare runs prepareChart once per (repo, reference), however many requests
// ask for it at the same time, and at most PrepareConcurrency at once because
// each one holds an upstream connection and, when dependencies are rewritten,
// the whole tarball.
func (m *Manifests) prepare(ctx context.Context, repo, reference string) *errors.RegError {
	key := repo + "\x00" + reference
	if rerr := m.failures.get(key); rerr != nil {
		return rerr
	}
	ch := m.prepares.DoChan(key, func() (_ interface{}, err error) {
		// singleflight re-panics on a new goroutine, where nothing can recover
		// it and the whole process would exit.
		defer func() {
			if r := recover(); r != nil {
				m.log.Printf("panic preparing %s %s: %v\n%s", repo, reference, r, debug.Stack())
				err = errors.RegErrInternal(fmt.Errorf("preparing %s %s failed", repo, reference))
			}
		}()
		// A caller that missed just before an earlier flight finished.
		if m.prepared(repo, reference) {
			return nil, nil
		}
		if rerr := m.failures.get(key); rerr != nil {
			return nil, rerr
		}
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareTimeout)
		defer cancel()
		select {
		case m.slots <- struct{}{}:
			defer func() { <-m.slots }()
		case <-workCtx.Done():
			return nil, &errors.RegError{
				Status:  http.StatusServiceUnavailable,
				Code:    "UNAVAILABLE",
				Message: fmt.Sprintf("too many chart preparations in progress for %s %s", repo, reference),
			}
		}
		rerr := m.prepareChart(workCtx, repo, reference)
		// A version constraint such as 1.0 is stored under the version it
		// resolved to; without caching the 404, every request would repeat the
		// download.
		if rerr == nil && !m.prepared(repo, reference) {
			rerr = resultNotFound(repo, reference)
		}
		if rerr != nil {
			if rerr.Status == http.StatusNotFound || rerr.Status == http.StatusForbidden {
				m.failures.set(key, rerr)
			}
			return nil, rerr
		}
		return nil, nil
	})
	select {
	case res := <-ch:
		if res.Err == nil {
			return nil
		}
		if rerr, ok := res.Err.(*errors.RegError); ok {
			return rerr
		}
		return errors.RegErrInternal(res.Err)
	case <-ctx.Done():
		// nginx's code for a client that went away; it is only ever logged.
		return &errors.RegError{Status: 499, Code: "CLIENT_CLOSED_REQUEST", Message: ctx.Err().Error()}
	}
}

func (m *Manifests) prepared(repo, reference string) bool {
	if reference == "" {
		return len(m.names(repo)) > 0
	}
	_, err := m.Read(repo, reference)
	return err == nil
}

func resultNotFound(repo, target string) *errors.RegError {
	return &errors.RegError{
		Status:  http.StatusNotFound,
		Code:    "MANIFEST_UNKNOWN",
		Message: fmt.Sprintf("Chart prepare's result not found: %v, %v", repo, target),
	}
}

func (m *Manifests) names(repo string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := m.manifests[repo]
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// failureCache remembers chart versions the upstream definitively answered
// with 404 or 403, so a sweep over a missing version reaches the upstream once
// per TTL. It is kept out of the index cache so a 404 sweep cannot evict parsed
// indexes, and so an entry is visible to the very next request (ristretto
// applies writes asynchronously and may drop them).
type failureCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]failure
}

type failure struct {
	err   *errors.RegError
	until time.Time
}

const maxFailures = 16384

func newFailureCache(ttl time.Duration) *failureCache {
	return &failureCache{ttl: ttl, m: map[string]failure{}}
}

func (c *failureCache) get(key string) *errors.RegError {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.m[key]
	if !ok {
		return nil
	}
	if time.Now().After(f.until) {
		delete(c.m, key)
		return nil
	}
	return f.err
}

func (c *failureCache) set(key string, err *errors.RegError) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.m) >= maxFailures {
		for k, f := range c.m {
			if now.After(f.until) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxFailures {
			return
		}
	}
	c.m[key] = failure{err: err, until: now.Add(c.ttl)}
}
