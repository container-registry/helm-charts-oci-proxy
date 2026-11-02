package manifest

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
)

// prepareTimeout bounds one shared chart preparation. The work is detached from
// the first caller's request, because every concurrent caller of the same
// version waits for it and the result is cached for all of them.
const prepareTimeout = 2 * time.Minute

func (m *Manifests) manifest(ctx context.Context, repo, target string) (*record, *errors.RegError) {
	if r := m.lookup(repo, target); r != nil && r.err == nil {
		return r, nil
	}
	if err := m.prepare(ctx, repo, target); err != nil {
		return nil, err
	}
	if r := m.lookup(repo, target); r != nil && r.err == nil {
		return r, nil
	}
	return nil, resultNotFound(repo, target)
}

// prepare runs prepareChart once per (repo, reference), however many requests
// ask for it at the same time, and at most PrepareConcurrency at once because
// each one holds an upstream connection.
func (m *Manifests) prepare(ctx context.Context, repo, reference string) *errors.RegError {
	if rerr := m.failed(repo, reference); rerr != nil {
		return rerr
	}
	ch := m.prepares.DoChan(repo+"\x00"+reference, func() (_ interface{}, err error) {
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
		if rerr := m.failed(repo, reference); rerr != nil {
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
				m.fail(repo, reference, rerr)
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
	r := m.lookup(repo, reference)
	return r != nil && r.err == nil
}

func (m *Manifests) failed(repo, reference string) *errors.RegError {
	if r := m.lookup(repo, reference); r != nil {
		return r.err
	}
	return nil
}

func resultNotFound(repo, target string) *errors.RegError {
	return &errors.RegError{
		Status:  http.StatusNotFound,
		Code:    "MANIFEST_UNKNOWN",
		Message: fmt.Sprintf("Chart prepare's result not found: %v, %v", repo, target),
	}
}
