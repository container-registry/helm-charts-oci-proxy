package cmd

import (
	"runtime"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/manifest"
	"github.com/dgraph-io/ristretto"
)

func heapGrowth(t *testing.T, f func() *ristretto.Cache) int64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	c := f()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(c)
	c.Close()
	return max(int64(after.HeapAlloc)-int64(before.HeapAlloc), 0)
}

// The previous NumCounters of 1e7 cost a fixed 48MiB of heap for a cache that
// holds one key per chart repository.
func TestIndexCacheFixedHeap(t *testing.T) {
	old := heapGrowth(t, func() *ristretto.Cache {
		c, err := ristretto.NewCache(&ristretto.Config{NumCounters: 1e7, MaxCost: 100000000, BufferItems: 64})
		if err != nil {
			t.Fatal(err)
		}
		return c
	})
	cur := heapGrowth(t, func() *ristretto.Cache {
		c, err := newIndexCache(128, false)
		if err != nil {
			t.Fatal(err)
		}
		return c
	})
	t.Logf("fixed heap: before %d MiB, now %d KiB", old>>20, cur>>10)
	if cur > 1<<20 {
		t.Errorf("index cache uses %d KiB at startup, want < 1 MiB", cur>>10)
	}
}

func TestIndexCacheAdmitsLargestIndex(t *testing.T) {
	const mib = 128
	c, err := newIndexCache(mib, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i, size := range []int{6_640_744, 1_819_369, 3_600_000, 1_900_000, 1_800_000} {
		c.SetWithTTL(i, i, manifest.IndexCost(size, nil, mib<<20), time.Hour)
	}
	c.SetWithTTL("bitnami", "bitnami", manifest.IndexCost(27_563_919, nil, mib<<20), time.Hour)
	c.Wait()
	if _, ok := c.Get("bitnami"); !ok {
		t.Error("bitnami index was not admitted")
	}
}

// An index larger than the whole budget is capped at the budget, so it is still
// cached instead of being downloaded on every request.
func TestIndexCacheAdmitsOversizedIndex(t *testing.T) {
	const mib = 128
	c, err := newIndexCache(mib, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetWithTTL("huge", "huge", manifest.IndexCost(200<<20, nil, mib<<20), time.Hour)
	c.Wait()
	if _, ok := c.Get("huge"); !ok {
		t.Error("index larger than the budget was not admitted")
	}
}

func TestNewIndexCacheRejectsInvalidSize(t *testing.T) {
	for _, mib := range []int{0, -1, maxIndexCacheMiB + 1} {
		if c, err := newIndexCache(mib, false); err == nil {
			c.Close()
			t.Errorf("size %d MiB accepted", mib)
		}
	}
}
