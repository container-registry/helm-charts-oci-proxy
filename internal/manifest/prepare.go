package manifest

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
)

// prepareTimeout bounds one shared chart preparation. The work is detached from
// the first caller's request, because every concurrent caller of the same
// version waits for it and the result is cached for all of them.
const prepareTimeout = 2 * time.Minute

const prepareErrorKeyPrefix = "prepare-error\x00"

// manifest returns the cached manifest for repo and target, preparing the chart
// first if needed. Requests for different versions run in parallel.
func (m *Manifests) manifest(ctx context.Context, repo, target string, opts RewriteOptions) (Manifest, *errors.RegError) {
	if ma, ok := m.lookup(repo, target); ok {
		return ma, nil
	}
	if err := m.prepare(ctx, repo, target, opts); err != nil {
		return Manifest{}, err
	}
	if ma, ok := m.lookup(repo, target); ok {
		return ma, nil
	}
	return Manifest{}, &errors.RegError{
		Status:  http.StatusNotFound,
		Code:    "MANIFEST_UNKNOWN",
		Message: fmt.Sprintf("Chart prepare's result not found: %v, %v", repo, target),
	}
}

// prepare runs prepareChart once per (repo, reference), however many requests
// ask for it at the same time. A failure is remembered for
// IndexErrorCacheTTl so a sweep over a missing version does not reach the
// upstream on every request.
func (m *Manifests) prepare(ctx context.Context, repo, reference string, opts RewriteOptions) *errors.RegError {
	key := repo + "\x00" + reference
	if v, ok := m.cache.Get(prepareErrorKeyPrefix + key); ok {
		if rerr, ok := v.(*errors.RegError); ok {
			return rerr
		}
	}
	ch := m.prepares.DoChan(key, func() (interface{}, error) {
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareTimeout)
		defer cancel()
		if rerr := m.prepareChart(workCtx, repo, reference, opts); rerr != nil {
			m.cache.SetWithTTL(prepareErrorKeyPrefix+key, rerr, errorEntryCost+int64(len(rerr.Message)), m.config.IndexErrorCacheTTl)
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
		return errors.RegErrInternal(ctx.Err())
	}
}

func (m *Manifests) lookup(repo, target string) (Manifest, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ma, ok := m.manifests[repo][target]
	return ma, ok
}

// names returns the manifest keys stored for repo, or nil if there are none.
func (m *Manifests) names(repo string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.manifests[repo]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}
