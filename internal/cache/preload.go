package cache

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// PreloadStats holds the results of a cache preload operation.
type PreloadStats struct {
	// Files is the number of files successfully loaded into cache.
	Files int
	// Bytes is the total raw (uncompressed) bytes loaded.
	Bytes int64
	// Skipped is the number of files skipped (too large, unreadable, etc.).
	Skipped int
	// Paths is the list of URL keys that were loaded into cache, for pre-warming
	// downstream caches (e.g. security.PathCache).
	Paths []string
}

// PreloadConfig controls preload behaviour. It mirrors the subset of the
// full config that the preload step needs, avoiding a circular import
// with the config package.
type PreloadConfig struct {
	// MaxFileSize is the maximum individual file size to preload.
	MaxFileSize int64
	// IndexFile is the index filename for directory requests (e.g. "index.html").
	IndexFile string
	// BlockDotfiles skips files whose path components start with ".".
	BlockDotfiles bool
	// CompressEnabled enables pre-compression of loaded files.
	CompressEnabled bool
	// CompressMinSize is the minimum file size for compression.
	CompressMinSize int
	// CompressLevel is the gzip compression level.
	CompressLevel int
	// CompressFn is an optional function that gzip-compresses src.
	// When nil, no gzip variants are produced.
	CompressFn func(src []byte, level int) ([]byte, error)
}

// Preload walks root and loads every eligible regular file into the cache.
// Files larger than cfg.MaxFileSize or whose path contains dotfile segments
// (when cfg.BlockDotfiles is true) are skipped. The function returns stats
// describing what was loaded.
func (c *Cache) Preload(root string, cfg PreloadConfig) PreloadStats {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return PreloadStats{}
	}
	if real, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = real
	}

	indexFile := cfg.IndexFile
	if indexFile == "" {
		indexFile = "index.html"
	}

	var stats PreloadStats

	_ = filepath.WalkDir(absRoot, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// Skip dotfile components.
		if cfg.BlockDotfiles {
			rel, relErr := filepath.Rel(absRoot, fpath)
			if relErr != nil {
				stats.Skipped++
				return nil
			}
			if hasDotfileSegment(rel) {
				stats.Skipped++
				return nil
			}
		}

		info, err := d.Info()
		if err != nil {
			stats.Skipped++
			return nil
		}

		// Skip files exceeding max size.
		if info.Size() > cfg.MaxFileSize {
			stats.Skipped++
			return nil
		}

		data, err := os.ReadFile(fpath)
		if err != nil {
			stats.Skipped++
			return nil
		}

		rel, err := filepath.Rel(absRoot, fpath)
		if err != nil {
			stats.Skipped++
			return nil
		}

		// Build the URL-style cache key.
		urlKey := "/" + filepath.ToSlash(rel)

		ct := detectMIMEType(fpath, data)
		etag := computeFileETag(data)

		cached := &CachedFile{
			Data:         data,
			ETag:         etag,
			ETagFull:     `W/"` + etag + `"`,
			LastModified: info.ModTime(),
			ContentType:  ct,
			Size:         info.Size(),
		}

		// Pre-compress if eligible.
		if cfg.CompressEnabled && cfg.CompressFn != nil &&
			isCompressibleType(ct) && len(data) >= cfg.CompressMinSize {
			if gz, err := cfg.CompressFn(data, cfg.CompressLevel); err == nil {
				cached.GzipData = gz
			}
		}

		cached.InitHeaders()

		c.Put(urlKey, cached)
		stats.Files++
		stats.Bytes += info.Size()
		stats.Paths = append(stats.Paths, urlKey)

		// Also register the directory path if this is the index file.
		if path.Base(urlKey) == indexFile {
			dir := path.Dir(urlKey)
			if dir != "/" {
				dir += "/"
			}
			c.Put(dir, cached)
			stats.Paths = append(stats.Paths, dir)
		}

		return nil
	})

	return stats
}

// hasDotfileSegment reports whether any path component starts with ".".
func hasDotfileSegment(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg != "" && strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// detectMIMEType returns the MIME type for a file, falling back to
// http.DetectContentType for unknown extensions.
func detectMIMEType(filePath string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	if data != nil {
		snippet := data
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return http.DetectContentType(snippet)
	}
	return "application/octet-stream"
}

// computeFileETag returns the first 16 hex characters of sha256(data).
func computeFileETag(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)[:16]
}

// isCompressibleType reports whether the MIME type is eligible for compression.
// This is a subset duplicated here to avoid importing the compress package.
var compressiblePrefixes = []string{
	"text/",
	"application/javascript",
	"application/json",
	"application/xml",
	"application/wasm",
	"image/svg+xml",
	"font/",
	"application/font-woff",
}

func isCompressibleType(ct string) bool {
	// Strip parameters.
	if idx := strings.Index(ct, ";"); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	ct = strings.ToLower(ct)
	for _, prefix := range compressiblePrefixes {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}
