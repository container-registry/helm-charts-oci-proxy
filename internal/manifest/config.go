package manifest

import "time"

type Config struct {
	Debug              bool
	CacheTTL           time.Duration // for how long store manifest
	IndexCacheTTL      time.Duration
	IndexCacheMaxCost  int64 // ristretto MaxCost of the index cache
	IndexErrorCacheTTl time.Duration
	// PrepareConcurrency bounds chart preparations running at once. Zero means
	// the default.
	PrepareConcurrency int

	// AllowPrivateNetworks permits upstream downloads from private, loopback,
	// and link-local addresses (e.g. proxying an internal chart repository).
	// Off by default as an SSRF guard.
	AllowPrivateNetworks bool

	// RewriteDependencies enables rewriting of chart dependency URLs to point
	// through the proxy. It applies to every request: prepared charts are shared
	// by all clients, so no request may choose it.
	RewriteDependencies bool
	// ProxyHost is the hostname used for rewritten dependency URLs. Rewriting
	// is off while it is empty.
	ProxyHost string
}
