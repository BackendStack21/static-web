// Package cache provides a size-bounded in-memory LRU cache for static files.
// It wraps github.com/hashicorp/golang-lru/v2 and tracks total byte usage
// to enforce a maximum memory ceiling independent of item count.
package cache

import (
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	// cacheOverhead is a conservative estimate of per-entry struct overhead in bytes.
	cacheOverhead = 256
	// maxLRUItems is the maximum number of entries the underlying LRU may hold.
	// Byte-level eviction happens before this is reached in practice.
	maxLRUItems = 65536
)

// CachedFile holds the content and metadata for a single cached file.
type CachedFile struct {
	// Data is the raw (uncompressed) file content.
	Data []byte
	// GzipData is the pre-compressed gzip content, or nil if unavailable.
	GzipData []byte
	// BrData is the pre-compressed brotli content, or nil if unavailable.
	BrData []byte
	// ETag is the first 16 hex characters of sha256(Data), without quotes.
	ETag string
	// ETagFull is the pre-formatted weak ETag ready for use in HTTP headers,
	// e.g. `W/"abc123"`. Pre-computing it avoids a per-request string alloc.
	ETagFull string
	// LastModified is the file's modification time.
	LastModified time.Time
	// ContentType is the detected MIME type.
	ContentType string
	// Size is the length of Data in bytes.
	Size int64
	// ExpiresAt is the cache entry expiry time. Zero means no expiry.
	ExpiresAt time.Time
}

// totalSize returns the approximate byte footprint of the entry.
func (f *CachedFile) totalSize() int64 {
	return int64(len(f.Data)+len(f.GzipData)+len(f.BrData)) + cacheOverhead
}

// CacheStats holds runtime statistics for the cache.
type CacheStats struct {
	// Hits is the total number of successful cache lookups.
	Hits int64
	// Misses is the total number of failed cache lookups.
	Misses int64
	// CurrentBytes is the current total byte usage.
	CurrentBytes int64
	// EntryCount is the current number of entries.
	EntryCount int
}

// Cache is a thread-safe, size-bounded LRU cache for CachedFile entries.
type Cache struct {
	lru      *lru.Cache[string, *CachedFile]
	mu       sync.Mutex
	maxBytes int64
	ttl      time.Duration
	curBytes atomic.Int64
	hits     atomic.Int64
	misses   atomic.Int64
}

// NewCache creates a new Cache with the given maximum byte capacity.
// If maxBytes is <= 0, a default of 256 MB is used.
// If ttl is provided and > 0, entries expire after that duration.
func NewCache(maxBytes int64, ttl ...time.Duration) *Cache {
	if maxBytes <= 0 {
		maxBytes = 256 * 1024 * 1024
	}

	c := &Cache{maxBytes: maxBytes}
	if len(ttl) > 0 && ttl[0] > 0 {
		c.ttl = ttl[0]
	}

	onEvict := func(_ string, f *CachedFile) {
		c.curBytes.Add(-f.totalSize())
	}

	// The item-count limit is generous; byte tracking enforces the real ceiling.
	l, err := lru.NewWithEvict[string, *CachedFile](maxLRUItems, onEvict)
	if err != nil {
		// NewWithEvict only errors on size <= 0, which can't happen here.
		panic("cache: failed to create LRU: " + err.Error())
	}
	c.lru = l
	return c
}

// Get returns the cached file for the given path key, or (nil, false) on miss.
func (c *Cache) Get(path string) (*CachedFile, bool) {
	f, ok := c.lru.Get(path)
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	if !f.ExpiresAt.IsZero() && time.Now().After(f.ExpiresAt) {
		c.lru.Remove(path)
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return f, ok
}

// Put stores a file in the cache under the given path key.
// If adding the new entry would exceed maxBytes, it evicts LRU entries
// until enough space is available. If the single entry is larger than
// maxBytes, it is not cached.
func (c *Cache) Put(path string, f *CachedFile) {
	newSize := f.totalSize()
	if newSize > c.maxBytes {
		// Single file exceeds capacity; don't cache.
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ttl > 0 {
		f.ExpiresAt = time.Now().Add(c.ttl)
	} else {
		f.ExpiresAt = time.Time{}
	}

	// If the key already exists, subtract its old size before adding.
	if old, ok := c.lru.Peek(path); ok {
		c.curBytes.Add(-old.totalSize())
	}

	// Evict LRU entries until we have enough room.
	for c.curBytes.Load()+newSize > c.maxBytes && c.lru.Len() > 0 {
		c.lru.RemoveOldest()
	}

	c.lru.Add(path, f)
	c.curBytes.Add(newSize)
}

// Flush removes all entries from the cache.
func (c *Cache) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.Purge()
	c.curBytes.Store(0)
}

// Stats returns a snapshot of current cache statistics.
func (c *Cache) Stats() CacheStats {
	return CacheStats{
		Hits:         c.hits.Load(),
		Misses:       c.misses.Load(),
		CurrentBytes: c.curBytes.Load(),
		EntryCount:   c.lru.Len(),
	}
}
