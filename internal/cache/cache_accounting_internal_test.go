package cache

import (
	"math/rand"
	"testing"
)

// liveFootprint returns, for every entry currently held by the LRU, the sum
// of its accounted size and the sum of its byte-slice capacities. It uses
// Keys/Peek so recency, hit counters and expiry are left untouched.
func liveFootprint(c *Cache) (accounted, caps int64) {
	for _, k := range c.lru.Keys() {
		f, ok := c.lru.Peek(k)
		if !ok {
			continue
		}
		accounted += f.totalSize()
		caps += int64(cap(f.Data) + cap(f.GzipData) + cap(f.BrData) + cap(f.ZstdData))
	}
	return accounted, caps
}

// checkAccounting asserts the cache's byte counter matches what is really
// held, that the real footprint respects maxBytes, and that the counter
// covers every retained backing array.
func checkAccounting(t *testing.T, c *Cache, step string) {
	t.Helper()
	accounted, caps := liveFootprint(c)
	cur := c.Stats().CurrentBytes
	if cur != accounted {
		t.Fatalf("%s: CurrentBytes = %d, sum over live entries = %d", step, cur, accounted)
	}
	if accounted > c.maxBytes {
		t.Fatalf("%s: live entries hold %d bytes, above maxBytes %d", step, accounted, c.maxBytes)
	}
	if cur < caps {
		t.Fatalf("%s: CurrentBytes = %d is below the %d bytes of backing arrays retained", step, cur, caps)
	}
}

func sized(n int) *CachedFile { return &CachedFile{Data: make([]byte, n)} }

// P-F8(b): replacing a key while the cache is full used to subtract the old
// entry's size twice (once in Put, once in onEvict when the eviction loop
// picked that same key), leaving CurrentBytes=6256 for a 10512-byte footprint.
func TestCachePut_ReplaceWhileFullKeepsAccountingExact(t *testing.T) {
	c := NewCache(10000)
	c.Put("/a", sized(4000))
	checkAccounting(t, c, "put /a 4000")
	c.Put("/b", sized(4000))
	checkAccounting(t, c, "put /b 4000")
	c.Put("/a", sized(6000))
	checkAccounting(t, c, "replace /a 6000")

	// Repeated rounds used to drift further each time.
	for round := 0; round < 20; round++ {
		c.Put("/b", sized(4000))
		checkAccounting(t, c, "round put /b")
		c.Put("/a", sized(4000))
		checkAccounting(t, c, "round put /a")
		c.Put("/a", sized(6000))
		checkAccounting(t, c, "round replace /a")
	}
}

// Replacing a key must also work when the replacement is smaller and when
// the old entry is not the LRU victim.
func TestCachePut_ReplaceKeepsAccountingExactUnderRandomWorkload(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	c := NewCache(20000)
	keys := []string{"/a", "/b", "/c", "/d", "/e", "/f"}
	for i := 0; i < 2000; i++ {
		k := keys[r.Intn(len(keys))]
		switch r.Intn(4) {
		case 0:
			if f, ok := c.lru.Peek(k); ok {
				c.removeIfSame(k, f)
			}
		default:
			n := r.Intn(9000)
			f := &CachedFile{Data: make([]byte, n, n+r.Intn(2000))}
			if r.Intn(2) == 0 {
				f.GzipData = make([]byte, n/4, n/4+r.Intn(500))
			}
			c.Put(k, f)
		}
		checkAccounting(t, c, "random op")
	}
}

// P-F8(a): the counter must reflect the retained backing arrays (cap), not
// just the visible bytes (len), for every byte field.
func TestCacheAccounting_CountsBackingCapacityOfAllVariants(t *testing.T) {
	c := NewCache(1 << 20)
	f := &CachedFile{
		Data:     make([]byte, 100, 1000),
		GzipData: make([]byte, 10, 500),
		BrData:   make([]byte, 10, 300),
		ZstdData: make([]byte, 10, 200),
	}
	c.Put("/f", f)
	checkAccounting(t, c, "put")
	if got, min := c.Stats().CurrentBytes, int64(1000+500+300+200); got < min {
		t.Fatalf("CurrentBytes = %d, want >= %d (sum of capacities)", got, min)
	}
}

// With capacity-based accounting the byte ceiling bounds real memory: files
// whose backing arrays are much larger than their contents must evict sooner.
func TestCacheAccounting_EvictsByCapacityNotLength(t *testing.T) {
	c := NewCache(4000)
	for _, k := range []string{"/a", "/b", "/c", "/d"} {
		c.Put(k, &CachedFile{Data: make([]byte, 100, 1500)})
		checkAccounting(t, c, "put "+k)
	}
	if n := c.Stats().EntryCount; n > 2 {
		t.Fatalf("EntryCount = %d, want <= 2 when each entry retains ~1.7 KB of a 4 KB budget", n)
	}
}
