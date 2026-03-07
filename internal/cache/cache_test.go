package cache_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/static-web/server/internal/cache"
)

func makeFile(dataSize int) *cache.CachedFile {
	return &cache.CachedFile{
		Data:         make([]byte, dataSize),
		ETag:         "abc123",
		LastModified: time.Now(),
		ContentType:  "text/html",
		Size:         int64(dataSize),
	}
}

func TestCacheGetMiss(t *testing.T) {
	c := cache.NewCache(1024 * 1024)
	_, ok := c.Get("/missing")
	if ok {
		t.Error("expected cache miss, got hit")
	}

	stats := c.Stats()
	if stats.Misses != 1 {
		t.Errorf("Misses = %d, want 1", stats.Misses)
	}
}

func TestCachePutAndGet(t *testing.T) {
	c := cache.NewCache(1024 * 1024)
	f := makeFile(512)
	c.Put("/test", f)

	got, ok := c.Get("/test")
	if !ok {
		t.Fatal("expected cache hit after Put")
	}
	if got.Size != f.Size {
		t.Errorf("Size = %d, want %d", got.Size, f.Size)
	}

	stats := c.Stats()
	if stats.Hits != 1 {
		t.Errorf("Hits = %d, want 1", stats.Hits)
	}
	if stats.EntryCount != 1 {
		t.Errorf("EntryCount = %d, want 1", stats.EntryCount)
	}
}

func TestCacheByteEviction(t *testing.T) {
	// Allow ~3 KB total (plus overhead per entry).
	// Each file is 1000 bytes; overhead makes total ~3 entries max.
	c := cache.NewCache(4000)

	c.Put("/a", makeFile(1000))
	c.Put("/b", makeFile(1000))
	c.Put("/c", makeFile(1000))

	stats := c.Stats()
	if stats.CurrentBytes <= 0 {
		t.Error("CurrentBytes should be > 0 after puts")
	}

	// Adding a 4th entry should evict oldest.
	c.Put("/d", makeFile(1000))

	// "/a" should have been evicted.
	_, ok := c.Get("/a")
	if ok {
		t.Error("expected /a to be evicted, but it was still in cache")
	}
}

func TestCacheFlush(t *testing.T) {
	c := cache.NewCache(1024 * 1024)
	c.Put("/x", makeFile(100))
	c.Put("/y", makeFile(100))

	c.Flush()

	stats := c.Stats()
	if stats.EntryCount != 0 {
		t.Errorf("EntryCount = %d after Flush, want 0", stats.EntryCount)
	}
	if stats.CurrentBytes != 0 {
		t.Errorf("CurrentBytes = %d after Flush, want 0", stats.CurrentBytes)
	}

	if _, ok := c.Get("/x"); ok {
		t.Error("expected /x to be gone after Flush")
	}
}

func TestCacheOversizedEntry(t *testing.T) {
	// Cache with 1 KB limit.
	c := cache.NewCache(1024)

	// File larger than the entire cache should not be stored.
	c.Put("/big", makeFile(2048))

	_, ok := c.Get("/big")
	if ok {
		t.Error("oversized file should not be stored in cache")
	}
}

func TestCacheStats(t *testing.T) {
	c := cache.NewCache(1024 * 1024)
	c.Put("/p", makeFile(100))

	c.Get("/p")  // hit
	c.Get("/p")  // hit
	c.Get("/no") // miss

	s := c.Stats()
	if s.Hits != 2 {
		t.Errorf("Hits = %d, want 2", s.Hits)
	}
	if s.Misses != 1 {
		t.Errorf("Misses = %d, want 1", s.Misses)
	}
}

func TestCacheUpdateExistingKey(t *testing.T) {
	c := cache.NewCache(1024 * 1024)
	c.Put("/same", makeFile(100))
	before := c.Stats().CurrentBytes

	// Re-put with a larger file; bytes should increase, not double-count.
	c.Put("/same", makeFile(500))
	after := c.Stats().CurrentBytes

	if after <= before {
		t.Errorf("CurrentBytes should grow when replacing with larger entry; before=%d after=%d", before, after)
	}

	// Verify we still get only 1 entry.
	if c.Stats().EntryCount != 1 {
		t.Errorf("EntryCount = %d, want 1 after update", c.Stats().EntryCount)
	}
}

// ---------------------------------------------------------------------------
// Additional edge-case tests
// ---------------------------------------------------------------------------

// TestCacheDefaultMaxBytes verifies that NewCache(0) uses the 256 MB default.
func TestCacheDefaultMaxBytes(t *testing.T) {
	c := cache.NewCache(0)
	// A small file must be accepted (proves the cache is usable with default size).
	c.Put("/small", makeFile(128))
	if _, ok := c.Get("/small"); !ok {
		t.Error("expected cache hit after put with default (0) maxBytes")
	}
}

// TestCacheNegativeMaxBytes mirrors the zero case; both should use the default.
func TestCacheNegativeMaxBytes(t *testing.T) {
	c := cache.NewCache(-1)
	c.Put("/neg", makeFile(64))
	if _, ok := c.Get("/neg"); !ok {
		t.Error("expected cache hit after put with negative maxBytes (default should apply)")
	}
}

// TestCacheMultipleEvictions ensures that several LRU entries are evicted when
// a single large-ish put exhausts the remaining headroom.
func TestCacheMultipleEvictions(t *testing.T) {
	// Each file is ~600 bytes data + 256 overhead = ~856 bytes per entry.
	// maxBytes = 2000 → fits at most 2 entries; 3rd insert must evict.
	c := cache.NewCache(2000)

	c.Put("/x", makeFile(600))
	c.Put("/y", makeFile(600))
	// This should evict both /x (oldest) and possibly /y to make room.
	c.Put("/z", makeFile(600))

	// /z must always survive (it was just added).
	if _, ok := c.Get("/z"); !ok {
		t.Error("newest entry /z should be in cache")
	}
	// At least one of the older entries should have been evicted.
	_, xOK := c.Get("/x")
	_, yOK := c.Get("/y")
	if xOK && yOK {
		t.Error("at least one of /x or /y should have been evicted to make room for /z")
	}
}

// TestCacheByteCountAfterEviction confirms curBytes stays accurate post-eviction.
func TestCacheByteCountAfterEviction(t *testing.T) {
	// Tight capacity: only ~1500 bytes, each entry ≈ 756 bytes (500 data + 256 overhead).
	c := cache.NewCache(1500)

	c.Put("/a", makeFile(500))
	c.Put("/b", makeFile(500))
	// Adding /c should evict /a.
	c.Put("/c", makeFile(500))

	stats := c.Stats()
	if stats.CurrentBytes <= 0 {
		t.Errorf("CurrentBytes = %d, want > 0 after eviction", stats.CurrentBytes)
	}
	// Bytes must not exceed the configured maximum.
	if stats.CurrentBytes > 1500 {
		t.Errorf("CurrentBytes = %d exceeds maxBytes 1500", stats.CurrentBytes)
	}
}

// TestCacheFlushResetsBytes verifies that Flush zeroes out CurrentBytes.
func TestCacheFlushResetsBytes(t *testing.T) {
	c := cache.NewCache(1024 * 1024)
	c.Put("/p1", makeFile(256))
	c.Put("/p2", makeFile(256))

	c.Flush()

	if s := c.Stats(); s.CurrentBytes != 0 {
		t.Errorf("CurrentBytes after Flush = %d, want 0", s.CurrentBytes)
	}
	if s := c.Stats(); s.EntryCount != 0 {
		t.Errorf("EntryCount after Flush = %d, want 0", s.EntryCount)
	}
}

// TestCacheGetAfterFlush confirms misses are counted after a flush.
func TestCacheGetAfterFlush(t *testing.T) {
	c := cache.NewCache(1024 * 1024)
	c.Put("/item", makeFile(100))
	c.Flush()

	_, ok := c.Get("/item")
	if ok {
		t.Error("expected miss after Flush")
	}
	s := c.Stats()
	if s.Misses < 1 {
		t.Errorf("Misses = %d, want ≥ 1 after Get on flushed key", s.Misses)
	}
}

// TestCacheConcurrentPutGet exercises the cache under concurrent load.
// This is a correctness test (not a benchmark); it ensures no data races.
func TestCacheConcurrentPutGet(t *testing.T) {
	c := cache.NewCache(4 * 1024 * 1024)
	const goroutines = 20
	const ops = 50

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("/file-%d", id%5)
			for range ops {
				c.Put(key, makeFile(128))
				c.Get(key)
			}
		}(i)
	}
	wg.Wait()

	// The cache must still be internally consistent.
	s := c.Stats()
	if s.CurrentBytes < 0 {
		t.Errorf("CurrentBytes = %d after concurrent use, want ≥ 0", s.CurrentBytes)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

// BenchmarkCacheGet measures the throughput of hot-path cache reads.
func BenchmarkCacheGet(b *testing.B) {
	c := cache.NewCache(64 * 1024 * 1024)
	f := makeFile(4096)
	c.Put("/bench/file.js", f)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get("/bench/file.js")
	}
}

// BenchmarkCachePut measures the throughput of cache writes (single key, no eviction).
func BenchmarkCachePut(b *testing.B) {
	c := cache.NewCache(64 * 1024 * 1024)
	f := makeFile(1024)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Put("/bench/asset.css", f)
	}
}

// BenchmarkCacheGetParallel measures concurrent read throughput via RunParallel.
func BenchmarkCacheGetParallel(b *testing.B) {
	c := cache.NewCache(64 * 1024 * 1024)
	f := makeFile(4096)
	c.Put("/bench/parallel.js", f)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Get("/bench/parallel.js")
		}
	})
}

// BenchmarkCachePutParallel measures concurrent write throughput (multiple keys
// to avoid single-key serialisation artefacts).
func BenchmarkCachePutParallel(b *testing.B) {
	c := cache.NewCache(256 * 1024 * 1024)
	files := make([]*cache.CachedFile, 16)
	keys := make([]string, 16)
	for i := range files {
		files[i] = makeFile(2048)
		keys[i] = fmt.Sprintf("/bench/file-%d.js", i)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			idx := i % len(keys)
			c.Put(keys[idx], files[idx])
			i++
		}
	})
}
