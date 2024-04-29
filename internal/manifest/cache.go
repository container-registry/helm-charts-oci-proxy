package manifest

import "time"

type Cache interface {
	SetWithTTL(key, value interface{}, cost int64, ttl time.Duration) bool
	Get(key interface{}) (interface{}, bool)
}
