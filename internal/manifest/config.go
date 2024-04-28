package manifest

import "time"

type Config struct {
	Debug              bool
	CacheTTL           time.Duration // for how long store manifest
	IndexCacheTTL      time.Duration
	IndexErrorCacheTTl time.Duration
}
