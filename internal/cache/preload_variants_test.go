package cache_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/klauspost/compress/zstd"
	"github.com/valyala/fasthttp"
)

const variantMinSize = 64

// variantPreloadConfig enables every variant the lazy serving path builds.
func variantPreloadConfig() cache.PreloadConfig {
	return cache.PreloadConfig{
		MaxFileSize:     1 << 20,
		IndexFile:       "index.html",
		BlockDotfiles:   true,
		CompressEnabled: true,
		Precompressed:   true,
		CompressMinSize: variantMinSize,
		CompressLevel:   5,
		CompressFn:      compress.GzipBytes,
		ZstdFn:          compress.ZstdBytes,
	}
}

func textBody(line string, n int) []byte {
	return []byte(strings.Repeat(line, n/len(line)+1)[:n])
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

func mustCached(t *testing.T, c *cache.Cache, key string) *cache.CachedFile {
	t.Helper()
	f, ok := c.Get(key)
	if !ok {
		t.Fatalf("%s not cached", key)
	}
	return f
}

func gunzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func unzstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	d, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	out, err := d.DecodeAll(data, nil)
	if err != nil {
		t.Fatalf("zstd decode: %v", err)
	}
	return out
}

// assertNoEntryContains fails if any cached entry (any key in paths, any
// variant) contains secret.
func assertNoEntryContains(t *testing.T, c *cache.Cache, paths []string, secret []byte) {
	t.Helper()
	for _, p := range paths {
		f, ok := c.Get(p)
		if !ok {
			continue
		}
		for name, b := range map[string][]byte{"Data": f.Data, "GzipData": f.GzipData, "BrData": f.BrData, "ZstdData": f.ZstdData} {
			if bytes.Contains(b, secret) {
				t.Errorf("%s %s contains the protected content", p, name)
			}
		}
	}
}

// P-F9: preload used to build only GzipData. It must load the .gz/.br/.zst
// sidecars the lazy path loads. The root is passed unresolved on purpose
// (on macOS t.TempDir() lives under the /var -> /private/var symlink).
func TestPreload_LoadsGzipBrotliAndZstdSidecarsAsVariants(t *testing.T) {
	root := t.TempDir()
	src := textBody("console.log('sidecar');\n", 4096)
	writeTestFile(t, filepath.Join(root, "js", "app.js"), src)
	writeTestFile(t, filepath.Join(root, "js", "app.js.gz"), []byte("GZ-SIDECAR"))
	writeTestFile(t, filepath.Join(root, "js", "app.js.br"), []byte("BR-SIDECAR"))
	writeTestFile(t, filepath.Join(root, "js", "app.js.zst"), []byte("ZST-SIDECAR"))

	c := cache.NewCache(10 << 20)
	c.Preload(root, variantPreloadConfig())

	f := mustCached(t, c, "/js/app.js")
	for name, got := range map[string][]byte{"GzipData": f.GzipData, "BrData": f.BrData, "ZstdData": f.ZstdData} {
		want := map[string]string{"GzipData": "GZ-SIDECAR", "BrData": "BR-SIDECAR", "ZstdData": "ZST-SIDECAR"}[name]
		if string(got) != want {
			t.Errorf("%s = %q, want sidecar %q", name, got, want)
		}
		if len(got) != cap(got) {
			t.Errorf("%s: len=%d cap=%d, want exact-size slice", name, len(got), cap(got))
		}
	}
}

func TestPreload_GeneratesGzipAndZstdWhenNoSidecars(t *testing.T) {
	root := t.TempDir()
	src := textBody("body { margin: 0; padding: 0; }\n", 3000)
	writeTestFile(t, filepath.Join(root, "style.css"), src)

	c := cache.NewCache(10 << 20)
	c.Preload(root, variantPreloadConfig())

	f := mustCached(t, c, "/style.css")
	if f.BrData != nil {
		t.Errorf("BrData = %d bytes, want nil (brotli is sidecar-only)", len(f.BrData))
	}
	if f.GzipData == nil || !bytes.Equal(gunzipBytes(t, f.GzipData), src) {
		t.Error("GzipData missing or does not decode to the source")
	}
	if f.ZstdData == nil || !bytes.Equal(unzstdBytes(t, f.ZstdData), src) {
		t.Error("ZstdData missing or does not decode to the source")
	}
	if len(f.Data) != cap(f.Data) {
		t.Errorf("Data: len=%d cap=%d, want exact-size slice", len(f.Data), cap(f.Data))
	}
}

func TestPreload_DoesNotCacheSidecarFilesAsEntries(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.js"), textBody("x();\n", 2000))
	for _, ext := range []string{".gz", ".br", ".zst"} {
		writeTestFile(t, filepath.Join(root, "app.js"+ext), []byte("SIDECAR"))
	}
	// Compressed files without a base file are ordinary assets.
	writeTestFile(t, filepath.Join(root, "orphan.css.br"), []byte("ORPHAN"))
	writeTestFile(t, filepath.Join(root, "backup.gz"), []byte("BACKUP"))

	c := cache.NewCache(10 << 20)
	st := c.Preload(root, variantPreloadConfig())

	for _, ext := range []string{".gz", ".br", ".zst"} {
		if _, ok := c.Get("/app.js" + ext); ok {
			t.Errorf("/app.js%s cached as its own entry", ext)
		}
	}
	for _, p := range st.Paths {
		if strings.HasPrefix(p, "/app.js.") {
			t.Errorf("stats.Paths lists sidecar %s", p)
		}
	}
	for _, key := range []string{"/app.js", "/orphan.css.br", "/backup.gz"} {
		mustCached(t, c, key)
	}
	if st.Files != 3 {
		t.Errorf("stats.Files = %d, want 3", st.Files)
	}
}

func TestPreload_IgnoresSidecarsWhenPrecompressedDisabled(t *testing.T) {
	root := t.TempDir()
	src := textBody("console.log('no sidecars');\n", 2000)
	writeTestFile(t, filepath.Join(root, "app.js"), src)
	writeTestFile(t, filepath.Join(root, "app.js.br"), []byte("BR-SIDECAR"))
	writeTestFile(t, filepath.Join(root, "app.js.gz"), []byte("GZ-SIDECAR"))

	cfg := variantPreloadConfig()
	cfg.Precompressed = false
	c := cache.NewCache(10 << 20)
	c.Preload(root, cfg)

	f := mustCached(t, c, "/app.js")
	if f.BrData != nil {
		t.Errorf("BrData = %q, want nil with precompressed disabled", f.BrData)
	}
	if !bytes.Equal(gunzipBytes(t, f.GzipData), src) {
		t.Error("GzipData should be generated from the source, not read from the sidecar")
	}
}

// Security: a sidecar that resolves to a dotfile must never be served as an
// encoded variant when dotfiles are blocked (the big.js.br -> .env leak).
func TestPreload_RejectsSidecarSymlinkToDotfile(t *testing.T) {
	setup := func(t *testing.T) (string, []byte, []byte) {
		root := t.TempDir()
		secret := []byte("DB_PASSWORD=hunter2")
		src := textBody("var a=1;\n", 400)
		writeTestFile(t, filepath.Join(root, ".env"), secret)
		writeTestFile(t, filepath.Join(root, ".secrets", "key"), secret)
		writeTestFile(t, filepath.Join(root, "big.js"), src)
		symlinkOrSkip(t, ".env", filepath.Join(root, "big.js.br"))
		symlinkOrSkip(t, filepath.Join(".secrets", "key"), filepath.Join(root, "big.js.zst"))
		return root, secret, src
	}

	t.Run("blocked", func(t *testing.T) {
		root, secret, src := setup(t)
		c := cache.NewCache(10 << 20)
		st := c.Preload(root, variantPreloadConfig())

		f := mustCached(t, c, "/big.js")
		if f.BrData != nil {
			t.Errorf("BrData = %q, want nil: sidecar resolves to .env", f.BrData)
		}
		if !bytes.Equal(unzstdBytes(t, f.ZstdData), src) {
			t.Error("ZstdData should be generated, not read from .secrets/key")
		}
		assertNoEntryContains(t, c, append(st.Paths, "/.env", "/big.js.br", "/big.js.zst"), secret)
	})

	// Control: the same fixture loads the sidecar when dotfiles are allowed,
	// so the blocked case above is rejected by the dotfile rule.
	t.Run("allowed", func(t *testing.T) {
		root, secret, _ := setup(t)
		cfg := variantPreloadConfig()
		cfg.BlockDotfiles = false
		c := cache.NewCache(10 << 20)
		c.Preload(root, cfg)
		if f := mustCached(t, c, "/big.js"); !bytes.Equal(f.BrData, secret) {
			t.Fatalf("control: BrData = %q, want the sidecar content", f.BrData)
		}
	})
}

// Security: a sidecar that resolves outside the root must be ignored,
// whether the symlink is absolute or relative.
func TestPreload_RejectsSidecarSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := []byte("TOP-SECRET-OUTSIDE")
	writeTestFile(t, filepath.Join(outside, "secret.br"), secret)
	src := textBody("console.log(1);\n", 2000)
	writeTestFile(t, filepath.Join(root, "app.js"), src)
	symlinkOrSkip(t, filepath.Join(outside, "secret.br"), filepath.Join(root, "app.js.br"))
	rel, err := filepath.Rel(root, filepath.Join(outside, "secret.br"))
	if err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, rel, filepath.Join(root, "app.js.gz"))

	c := cache.NewCache(10 << 20)
	st := c.Preload(root, variantPreloadConfig())

	f := mustCached(t, c, "/app.js")
	if f.BrData != nil {
		t.Errorf("BrData = %q, want nil: sidecar escapes root", f.BrData)
	}
	if !bytes.Equal(gunzipBytes(t, f.GzipData), src) {
		t.Error("GzipData should be generated, not read from outside the root")
	}
	assertNoEntryContains(t, c, st.Paths, secret)
}

func TestPreload_AcceptsSidecarSymlinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.js"), textBody("console.log(2);\n", 2000))
	writeTestFile(t, filepath.Join(root, "precompressed", "app.br"), []byte("BR-INSIDE"))
	symlinkOrSkip(t, filepath.Join("precompressed", "app.br"), filepath.Join(root, "app.js.br"))

	c := cache.NewCache(10 << 20)
	c.Preload(root, variantPreloadConfig())
	if f := mustCached(t, c, "/app.js"); string(f.BrData) != "BR-INSIDE" {
		t.Errorf("BrData = %q, want the in-root sidecar", f.BrData)
	}
}

func TestPreload_RejectsSidecarLargerThanSource(t *testing.T) {
	root := t.TempDir()
	src := textBody("console.log(3);\n", 1000)
	writeTestFile(t, filepath.Join(root, "app.js"), src)
	writeTestFile(t, filepath.Join(root, "app.js.br"), bytes.Repeat([]byte("B"), len(src)+1))
	writeTestFile(t, filepath.Join(root, "app.js.zst"), bytes.Repeat([]byte("Z"), len(src)))

	c := cache.NewCache(10 << 20)
	c.Preload(root, variantPreloadConfig())

	f := mustCached(t, c, "/app.js")
	if f.BrData != nil {
		t.Errorf("BrData = %d bytes, want nil: sidecar larger than source", len(f.BrData))
	}
	if len(f.ZstdData) != len(src) || f.ZstdData[0] != 'Z' {
		t.Error("a sidecar equal in size to the source should be accepted (same as the lazy path)")
	}
}

// Robustness: only non-empty regular files are sidecars. (FIFO sidecars are
// covered by TestPreload_NeverOpensFIFOSidecar on Unix.)
func TestPreload_RejectsNonRegularAndEmptySidecars(t *testing.T) {
	root := t.TempDir()
	src := textBody("console.log(4);\n", 2000)
	writeTestFile(t, filepath.Join(root, "app.js"), src)
	if err := os.Mkdir(filepath.Join(root, "app.js.br"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "app.js.zst"), nil)

	c := cache.NewCache(10 << 20)
	c.Preload(root, variantPreloadConfig())

	f := mustCached(t, c, "/app.js")
	if f.BrData != nil {
		t.Errorf("BrData = %d bytes, want nil: sidecar is a directory", len(f.BrData))
	}
	if !bytes.Equal(unzstdBytes(t, f.ZstdData), src) {
		t.Error("ZstdData should be generated: the .zst sidecar is empty")
	}
}

// The duplicated compressible-type list in package cache must agree with
// compress.IsCompressible, which the lazy serving path uses.
func TestFillVariants_UsesSameCompressibleTypesAsCompressPackage(t *testing.T) {
	types := []string{
		"text/html", "text/html; charset=utf-8", "TEXT/CSS", "text/plain", "text/xml",
		"text/javascript; charset=utf-8", "application/javascript", "application/json",
		"application/xml", "application/wasm", "image/svg+xml", "font/ttf", "font/otf",
		"font/woff", "application/font-woff", "font/woff2", "text/csv", "text/markdown",
		"text/event-stream", "image/png", "application/octet-stream", "application/zip",
		"application/json+ld", "", " text/html", "text/html ;q=1",
	}
	data := textBody("compressible? ", 4096)
	for _, ct := range types {
		f := &cache.CachedFile{Data: data, ContentType: ct}
		cache.FillVariants(f, "/nonexistent/file", "/nonexistent", cache.VariantConfig{
			Enabled: true, MinSize: 1, GzipLevel: 5, GzipFn: compress.GzipBytes,
		})
		if got, want := f.GzipData != nil, compress.IsCompressible(ct); got != want {
			t.Errorf("content type %q: cache compresses=%v, compress.IsCompressible=%v", ct, got, want)
		}
	}
}

// P-F9: a preloaded entry must carry the same variants (and bytes) as the
// entry the lazy handler path builds for the same file and configuration.
func TestPreload_VariantsMatchLazyHandlerPath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"app.js":       textBody("console.log('parity');\n", 4000),
		"app.js.br":    []byte("BR-SIDECAR"),
		"style.css":    textBody("a { color: red; }\n", 3000),
		"style.css.gz": []byte("GZ-SIDECAR"),
		"logo.svg":     textBody("<svg><path d='M0 0'/></svg>\n", 2000),
		"logo.svg.zst": []byte("ZST-SIDECAR"),
		"data.json":    textBody(`{"k":"v"},`, 2000),
		"notes.txt":    textBody("plain text notes\n", 2000),
		"page.html":    textBody("<p>hello</p>\n", 2000),
		"table.csv":    textBody("a,b,c\n", 2000),
		"font.woff2":   textBody("wOF2", 2000),
		"image.png":    textBody("\x89PNG\r\n\x1a\n", 2000),
		"tiny.js":      []byte("x();"),
	}
	for name, data := range files {
		writeTestFile(t, filepath.Join(root, name), data)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 10 << 20
	cfg.Cache.MaxFileSize = 1 << 20
	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = variantMinSize
	cfg.Compression.Level = 5
	cfg.Compression.Precompressed = true
	cfg.Security.BlockDotfiles = true
	cfg.Headers.StaticMaxAge = 3600

	lazy := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandlerQuiet(cfg, lazy)

	pcfg := variantPreloadConfig()
	pcfg.MaxFileSize = cfg.Cache.MaxFileSize
	pcfg.CompressMinSize = cfg.Compression.MinSize
	pcfg.CompressLevel = cfg.Compression.Level
	pre := cache.NewCache(cfg.Cache.MaxBytes)
	pre.Preload(root, pcfg)

	for name := range files {
		if strings.HasSuffix(name, ".br") || strings.HasSuffix(name, ".gz") || strings.HasSuffix(name, ".zst") {
			continue
		}
		key := "/" + name
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod("GET")
		ctx.Request.SetRequestURI(key)
		ctx.Request.Header.Set("Accept-Encoding", "br, zstd, gzip")
		h(&ctx)
		if sc := ctx.Response.StatusCode(); sc != fasthttp.StatusOK {
			t.Fatalf("GET %s: status %d", key, sc)
		}

		lf := mustCached(t, lazy, key)
		pf := mustCached(t, pre, key)
		if lf.ContentType != pf.ContentType || lf.ETag != pf.ETag || !bytes.Equal(lf.Data, pf.Data) {
			t.Errorf("%s: base entry differs (ct %q vs %q)", key, lf.ContentType, pf.ContentType)
		}
		for _, v := range []struct {
			name      string
			lazy, pre []byte
		}{
			{"GzipData", lf.GzipData, pf.GzipData},
			{"BrData", lf.BrData, pf.BrData},
			{"ZstdData", lf.ZstdData, pf.ZstdData},
		} {
			if (v.lazy == nil) != (v.pre == nil) || !bytes.Equal(v.lazy, v.pre) {
				t.Errorf("%s %s: lazy=%d bytes (nil=%v), preload=%d bytes (nil=%v)",
					key, v.name, len(v.lazy), v.lazy == nil, len(v.pre), v.pre == nil)
			}
		}
	}
}
