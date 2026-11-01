package cmd

import (
	"runtime"
	"testing"
	"time"

	"github.com/dgraph-io/ristretto"
)

func heapAfter(t *testing.T, f func() *ristretto.Cache) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	c := f()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(c)
	return after.HeapAlloc - before.HeapAlloc
}

// The previous NumCounters of 1e7 cost a fixed 48MiB of heap for a cache that
// holds one key per chart repository.
func TestIndexCacheFixedHeap(t *testing.T) {
	newCache := func(counters int64) func() *ristretto.Cache {
		return func() *ristretto.Cache {
			c, err := ristretto.NewCache(&ristretto.Config{NumCounters: counters, MaxCost: 128 << 20, BufferItems: 64})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	old, cur := heapAfter(t, newCache(1e7)), heapAfter(t, newCache(1e4))
	t.Logf("fixed heap: NumCounters 1e7 = %d MiB, 1e4 = %d KiB", old>>20, cur>>10)
	if cur > 1<<20 {
		t.Errorf("NumCounters 1e4 uses %d KiB, want < 1 MiB", cur>>10)
	}
}

// The largest public index (bitnami, 27.5MB, costed at 2x) must be admitted by
// the default cache size, together with a typical working set of smaller ones.
func TestIndexCacheAdmitsLargestIndex(t *testing.T) {
	c, err := ristretto.NewCache(&ristretto.Config{NumCounters: 1e4, MaxCost: 128 << 20, BufferItems: 64})
	if err != nil {
		t.Fatal(err)
	}
	for i, size := range []int64{6_640_744, 1_819_369, 3_600_000, 1_900_000, 1_800_000} {
		c.SetWithTTL(i, i, 2*size, time.Hour)
	}
	c.SetWithTTL("bitnami", "bitnami", 2*27_563_919, time.Hour)
	c.Wait()
	if _, ok := c.Get("bitnami"); !ok {
		t.Fatal("bitnami index was not admitted")
	}
}
