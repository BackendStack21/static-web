// Package cache provides a size-bounded in-memory LRU cache for static files.
// It wraps github.com/hashicorp/golang-lru/v2 and tracks total byte usage
// to enforce a maximum memory ceiling independent of item count.
package cache

import (
	"path"
	"strconv"
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

	// HTTPTimeFormat is the time format used for HTTP Date, Last-Modified,
	// and Expires headers. It is identical to the value of net/http.TimeFormat
	// but defined here to avoid importing net/http in the cache package.
	HTTPTimeFormat = "Mon, 02 Jan 2006 15:04:05 GMT"
)

// CachedFile holds the content and metadata for a single cached file.
type CachedFile struct {
	// Data is the raw (uncompressed) file content.
	Data []byte
	// GzipData is the pre-compressed gzip content, or nil if unavailable.
	GzipData []byte
	// BrData is the pre-compressed brotli content, or nil if unavailable.
	BrData []byte
	// ZstdData is the pre-compressed zstd content, or nil if unavailable.
	ZstdData []byte
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

	// Pre-formatted header values avoid per-request string formatting.
	// These are populated by InitHeaders() or by the preload path.
	// With fasthttp, headers are set via Set(key, value) taking plain strings;
	// an empty string means "not initialised".
	CTHeader string // e.g. "text/html; charset=utf-8"
	CLHeader string // e.g. "2943" — raw data Content-Length

	// Pre-formatted cache/conditional headers (PERF-003).
	// Populated by InitHeaders(); the serving hot path assigns these
	// directly to the response header, skipping all formatting.
	ETagHeader         string // e.g. `W/"abc123"`
	LastModHeader      string // e.g. "Mon, 15 Jan 2024 10:00:00 GMT"
	VaryHeader         string // e.g. "Accept-Encoding"
	CacheControlHeader string // e.g. "public, max-age=3600"
}

// InitHeaders pre-formats the Content-Type, Content-Length, ETag,
// Last-Modified, and Vary header strings so that the serving hot path can
// assign them directly without allocating (PERF-003).
// This is idempotent.
func (f *CachedFile) InitHeaders() {
	if f.CTHeader == "" {
		f.CTHeader = f.ContentType
	}
	if f.CLHeader == "" {
		f.CLHeader = strconv.FormatInt(f.Size, 10)
	}
	if f.ETagHeader == "" {
		etag := f.ETagFull
		if etag == "" {
			etag = `W/"` + f.ETag + `"`
		}
		f.ETagHeader = etag
	}
	if f.LastModHeader == "" {
		f.LastModHeader = f.LastModified.UTC().Format(HTTPTimeFormat)
	}
	if f.VaryHeader == "" {
		f.VaryHeader = "Accept-Encoding"
	}
}

// InitCacheControl pre-formats the Cache-Control header for a specific URL
// path and header configuration. This must be called after InitHeaders().
// urlPath is used to determine HTML vs static max-age; isHTML reports whether
// the file is an HTML document; immutablePattern is the glob for immutable files.
func (f *CachedFile) InitCacheControl(urlPath string, htmlMaxAge, staticMaxAge int, immutablePattern string) {
	if f.CacheControlHeader != "" {
		return
	}
	maxAge := staticMaxAge
	if isHTMLContent(urlPath, f.ContentType) {
		maxAge = htmlMaxAge
	}
	if maxAge == 0 {
		f.CacheControlHeader = "no-cache"
	} else {
		cc := "public, max-age=" + strconv.Itoa(maxAge)
		if immutablePattern != "" && matchesImmutable(urlPath, immutablePattern) {
			cc += ", immutable"
		}
		f.CacheControlHeader = cc
	}
}

// isHTMLContent reports whether the given URL path + content type indicates HTML.
func isHTMLContent(urlPath, contentType string) bool {
	if len(contentType) >= 9 {
		// Fast prefix check before calling strings functions.
		if contentType[0] == 't' && contentType[4] == '/' && contentType[5] == 'h' {
			return true // "text/html..."
		}
	}
	// Fallback to extension check.
	for i := len(urlPath) - 1; i >= 0; i-- {
		if urlPath[i] == '.' {
			ext := urlPath[i:]
			return ext == ".html" || ext == ".htm" ||
				ext == ".HTML" || ext == ".HTM"
		}
	}
	return false
}

// matchesImmutable checks if the base filename matches the immutable glob.
func matchesImmutable(urlPath, pattern string) bool {
	// Extract base name without filepath.Base allocation.
	base := urlPath
	if i := len(urlPath) - 1; i >= 0 {
		for i >= 0 && urlPath[i] != '/' {
			i--
		}
		base = urlPath[i+1:]
	}
	// filepath.Match is unavoidable for glob support but is only called
	// at cache-population time, never on the hot path.
	matched, _ := path.Match(pattern, base)
	return matched
}

// totalSize returns the approximate byte footprint of the entry.
func (f *CachedFile) totalSize() int64 {
	return int64(len(f.Data)+len(f.GzipData)+len(f.BrData)+len(f.ZstdData)) + cacheOverhead
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
