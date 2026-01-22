package manifest

import "time"

type Config struct {
	Debug              bool
	CacheTTL           time.Duration // for how long store manifest
	IndexCacheTTL      time.Duration
	IndexErrorCacheTTl time.Duration

	// RewriteDependencies enables rewriting of chart dependency URLs to point through the proxy
	RewriteDependencies bool
	// ProxyHost is the hostname used for rewritten dependency URLs.
	// If empty, the Host header from the incoming request is used.
	ProxyHost string
}
