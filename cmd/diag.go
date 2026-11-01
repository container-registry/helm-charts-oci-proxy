package cmd

import (
	"expvar"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/dgraph-io/ristretto"
)

// newDiagServer serves pprof, execution traces and expvar on their own
// listener so they are never reachable through the public ingress.
func newDiagServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/vars", expvar.Handler())
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func publishCacheMetrics(name string, c *ristretto.Cache) {
	expvar.Publish(name, expvar.Func(func() any {
		m := c.Metrics
		return map[string]uint64{
			"hits":          m.Hits(),
			"misses":        m.Misses(),
			"keys_added":    m.KeysAdded(),
			"keys_evicted":  m.KeysEvicted(),
			"sets_dropped":  m.SetsDropped(),
			"sets_rejected": m.SetsRejected(),
			"cost_added":    m.CostAdded(),
			"cost_evicted":  m.CostEvicted(),
		}
	}))
}
