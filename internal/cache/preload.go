package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
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
	// Skipped is the number of files skipped (too large, unreadable, a
	// sidecar of another file, etc.).
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
	// BlockDotfiles skips files whose path components start with ".". It also
	// rejects sidecar files that resolve to a dotfile.
	BlockDotfiles bool
	// CompressEnabled enables pre-compression of loaded files.
	CompressEnabled bool
	// CompressMinSize is the minimum file size for compression.
	CompressMinSize int
	// CompressLevel is the gzip compression level.
	CompressLevel int
	// CompressFn is an optional function that gzip-compresses src.
	// When nil, no gzip variants are generated.
	CompressFn func(src []byte, level int) ([]byte, error)
	// Precompressed loads the .gz, .br and .zst sidecar files next to each
	// compressible file as its encoded variants, as the lazy serving path does
	// when compression.precompressed is set. Sidecars are validated by
	// LoadSidecar.
	Precompressed bool
	// ZstdFn is an optional function that zstd-compresses src.
	// When nil, no zstd variants are generated.
	ZstdFn func(src []byte) ([]byte, error)

	// Header config for pre-computing Cache-Control at preload time (PERF-003).
	HTMLMaxAge       int
	StaticMaxAge     int
	ImmutablePattern string
}

// variantConfig returns the variant settings implied by the preload config.
func (cfg PreloadConfig) variantConfig() VariantConfig {
	return VariantConfig{
		Enabled:       cfg.CompressEnabled,
		Precompressed: cfg.Precompressed,
		MinSize:       cfg.CompressMinSize,
		GzipLevel:     cfg.CompressLevel,
		BlockDotfiles: cfg.BlockDotfiles,
		GzipFn:        cfg.CompressFn,
		ZstdFn:        cfg.ZstdFn,
	}
}

// Preload walks root and loads every eligible regular file into the cache.
// Files larger than cfg.MaxFileSize or whose path contains dotfile segments
// (when cfg.BlockDotfiles is true) are skipped, as are .gz/.br/.zst files
// whose base file exists (they are loaded as that file's variants instead).
// Each entry gets the same encoded variants the lazy serving path builds
// (see FillVariants). The function returns stats describing what was loaded.
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
	vcfg := cfg.variantConfig()

	var stats PreloadStats

	_ = filepath.WalkDir(absRoot, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// A sidecar next to its base file is that file's encoded variant, not
		// an asset of its own: caching it as well would hold its bytes twice.
		// Requests for it directly are still served (and cached) on demand.
		if isSidecarOfExistingFile(fpath) {
			stats.Skipped++
			return nil
		}

		// SEC-016: Validate symlink targets. If the entry is a symlink,
		// resolve it and verify the target is still inside the root directory.
		// This prevents preloading files that symlink outside the served root.
		if d.Type()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(fpath)
			if err != nil {
				stats.Skipped++
				return nil
			}
			rootWithSep := absRoot
			if !strings.HasSuffix(rootWithSep, string(filepath.Separator)) {
				rootWithSep += string(filepath.Separator)
			}
			if target != absRoot && !strings.HasPrefix(target, rootWithSep) {
				stats.Skipped++ // symlink escapes root
				return nil
			}
			fpath = target
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

		// fpath is now free of symlinks (a walk path under the resolved root,
		// or an EvalSymlinks result). Read the target itself, not the
		// directory entry: for symlinks d.Info() describes the link (Lstat).
		data, info, err := readRegularFile(fpath, cfg.MaxFileSize)
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

		FillVariants(cached, fpath, absRoot, vcfg)

		cached.InitHeaders()
		cached.InitCacheControl(urlKey, cfg.HTMLMaxAge, cfg.StaticMaxAge, cfg.ImmutablePattern)

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

// VariantConfig controls which encoded variants FillVariants attaches to a
// cached file. The encoders are injected because package compress imports
// this package (through headers), so cache cannot import compress.
type VariantConfig struct {
	// Enabled turns encoded variants on; when false FillVariants does nothing.
	Enabled bool
	// Precompressed loads .gz, .br and .zst sidecars next to the source file.
	Precompressed bool
	// MinSize is the minimum source size, in bytes, for any variant.
	MinSize int
	// GzipLevel is the level passed to GzipFn.
	GzipLevel int
	// BlockDotfiles rejects sidecars that resolve to a path with a dotfile
	// segment.
	BlockDotfiles bool
	// GzipFn generates the gzip variant when no .gz sidecar was loaded.
	// When nil, no gzip variant is generated.
	GzipFn func(src []byte, level int) ([]byte, error)
	// ZstdFn generates the zstd variant when no .zst sidecar was loaded.
	// When nil, no zstd variant is generated.
	ZstdFn func(src []byte) ([]byte, error)
}

// FillVariants attaches encoded variants to f the way the lazy serving path
// (the handler's serveFromDisk) does, so preloaded and on-demand entries are
// the same. f.Data and f.ContentType must already be set. Nothing happens
// unless cfg.Enabled is set, the content type is compressible and f.Data has
// at least cfg.MinSize bytes. If cfg.Precompressed is set, the srcPath+".gz",
// ".br" and ".zst" sidecars are then loaded through LoadSidecar, limited to
// len(f.Data) bytes. Any gzip or zstd variant still missing is generated with
// cfg.GzipFn or cfg.ZstdFn. Brotli is only ever loaded from a sidecar.
//
// srcPath must be the symlink-resolved path of the file f.Data was read
// from, and realRoot the symlink-resolved root directory.
func FillVariants(f *CachedFile, srcPath, realRoot string, cfg VariantConfig) {
	if !cfg.Enabled || len(f.Data) < cfg.MinSize || !isCompressibleType(f.ContentType) {
		return
	}
	if cfg.Precompressed {
		limit := int64(len(f.Data))
		f.GzipData = LoadSidecar(srcPath+".gz", realRoot, limit, cfg.BlockDotfiles)
		f.BrData = LoadSidecar(srcPath+".br", realRoot, limit, cfg.BlockDotfiles)
		f.ZstdData = LoadSidecar(srcPath+".zst", realRoot, limit, cfg.BlockDotfiles)
	}
	if f.GzipData == nil && cfg.GzipFn != nil {
		if gz, err := cfg.GzipFn(f.Data, cfg.GzipLevel); err == nil {
			f.GzipData = gz
		}
	}
	if f.ZstdData == nil && cfg.ZstdFn != nil {
		if zst, err := cfg.ZstdFn(f.Data); err == nil {
			f.ZstdData = zst
		}
	}
}

// LoadSidecar returns the contents of the precompressed sidecar file at
// sidecarPath, or nil if the file is missing or must not be served. After
// symlinks are resolved, the file must meet all of these conditions:
//   - It lies strictly inside realRoot.
//   - It has no dotfile path segment when blockDotfiles is set, so a
//     big.js.br -> .env link is refused.
//   - It is a non-empty regular file, never a FIFO or device that could
//     block or stream forever.
//   - It is no larger than maxSize bytes.
//
// realRoot must already be symlink-resolved (filepath.EvalSymlinks); an
// unresolved root makes every sidecar fail the containment check. The
// returned slice has len == cap.
func LoadSidecar(sidecarPath, realRoot string, maxSize int64, blockDotfiles bool) []byte {
	// Most files have no sidecars: one Lstat answers that far more cheaply
	// than EvalSymlinks, which walks every path component. Everything that
	// does exist goes through the full validation below.
	if _, err := os.Lstat(sidecarPath); err != nil {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(sidecarPath)
	if err != nil {
		return nil
	}
	rel, ok := relInsideRoot(realRoot, resolved)
	if !ok {
		return nil
	}
	if blockDotfiles && hasDotfileSegment(rel) {
		return nil
	}
	data, _, err := readRegularFile(resolved, maxSize)
	if err != nil || len(data) == 0 {
		return nil
	}
	return data
}

// relInsideRoot returns p relative to root when p lies strictly inside root.
// Both paths must be absolute and clean.
func relInsideRoot(root, p string) (string, bool) {
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if len(p) <= len(prefix) || !strings.HasPrefix(p, prefix) {
		return "", false
	}
	return p[len(prefix):], true
}

var (
	errNotRegular  = errors.New("cache: not a regular file")
	errTooLarge    = errors.New("cache: file exceeds the size limit")
	errFileChanged = errors.New("cache: file changed while being read")
)

// Errors returned by ReadRegularFile.
var (
	ErrNotRegular  = errNotRegular
	ErrTooLarge    = errTooLarge
	ErrFileChanged = errFileChanged
)

// ReadRegularFile is readRegularFile for the lazy serving path, so that a
// file read into the cache on demand gets the same checks as preload: a
// regular file of at most maxSize bytes that did not change while it was
// read, in an exact-size buffer. p must be symlink-resolved.
func ReadRegularFile(p string, maxSize int64) ([]byte, os.FileInfo, error) {
	return readRegularFile(p, maxSize)
}

// readRegularFile reads the file at p if it is a regular file of at most
// maxSize bytes. p must contain no symlinks: use a path from
// filepath.EvalSymlinks, or one from a walk of a resolved root.
//
// The type is checked with Lstat before the file is opened, because opening
// a FIFO blocks until a writer appears. The open handle is checked again so
// the bytes read come from the file that passed the first check. The contents
// are read into an exact-size buffer (len == cap). If the file shrinks or
// grows during the read, readRegularFile returns an error.
func readRegularFile(p string, maxSize int64) ([]byte, os.FileInfo, error) {
	pre, err := os.Lstat(p)
	if err != nil {
		return nil, nil, err
	}
	if !pre.Mode().IsRegular() {
		return nil, nil, errNotRegular
	}
	if pre.Size() > maxSize {
		return nil, nil, errTooLarge
	}

	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(pre, info) {
		return nil, nil, errFileChanged
	}
	if info.Size() > maxSize {
		return nil, nil, errTooLarge
	}

	data := make([]byte, info.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, nil, errFileChanged // shrank
	}
	var probe [1]byte
	if n, _ := f.Read(probe[:]); n != 0 {
		return nil, nil, errFileChanged // grew
	}
	return data, info, nil
}

// sidecarExts are the precompressed sidecar suffixes the serving path probes.
var sidecarExts = [...]string{".gz", ".br", ".zst"}

// isSidecarOfExistingFile reports whether p names a precompressed sidecar
// (.gz, .br or .zst) whose base file exists as a regular file.
func isSidecarOfExistingFile(p string) bool {
	for _, ext := range sidecarExts {
		if base, ok := strings.CutSuffix(p, ext); ok {
			info, err := os.Stat(base)
			return err == nil && info.Mode().IsRegular()
		}
	}
	return false
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
// Uses hex.EncodeToString on the first 8 bytes instead of fmt.Sprintf
// to avoid formatting the full 32-byte hash and then truncating (PERF-004).
func computeFileETag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// compressibleTypes mirrors the list behind compress.IsCompressible, which
// the lazy serving path uses. It is duplicated because package compress
// imports cache. TestFillVariants_UsesSameCompressibleTypesAsCompressPackage
// keeps the two in sync.
var compressibleTypes = map[string]bool{
	"text/html":              true,
	"text/css":               true,
	"text/plain":             true,
	"text/xml":               true,
	"text/javascript":        true,
	"application/javascript": true,
	"application/json":       true,
	"application/xml":        true,
	"application/wasm":       true,
	"image/svg+xml":          true,
	"font/ttf":               true,
	"font/otf":               true,
	"font/woff":              true,
	"application/font-woff":  true,
}

// isCompressibleType reports whether the MIME type is eligible for
// compression, with exactly the semantics of compress.IsCompressible.
func isCompressibleType(contentType string) bool {
	ct := contentType
	if idx := strings.IndexByte(ct, ';'); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	return compressibleTypes[strings.ToLower(ct)]
}
