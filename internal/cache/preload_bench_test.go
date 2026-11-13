package cache_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/security"
)

// preloadBenchRoot builds the performance-review preload fixture:
// 2000 JavaScript files of 8 KB spread across 50 directories.
func preloadBenchRoot(b *testing.B) string {
	b.Helper()
	root := b.TempDir()
	for d := 0; d < 50; d++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", d))
		if err := os.Mkdir(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		for f := 0; f < 40; f++ {
			line := fmt.Sprintf("console.log('module %d/%d: hello world, this is a benchmark asset');\n", d, f)
			body := strings.Repeat(line, 8192/len(line)+1)[:8192]
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.js", f)), []byte(body), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	return root
}

// gzipOnlyPreloadConfig mirrors what cmd/static-web passed to Preload at the
// time of the 2026-11 review (gzip variants only).
func gzipOnlyPreloadConfig() cache.PreloadConfig {
	return cache.PreloadConfig{
		MaxFileSize:     10 << 20,
		IndexFile:       "index.html",
		BlockDotfiles:   true,
		CompressEnabled: true,
		CompressMinSize: 1024,
		CompressLevel:   5,
		CompressFn:      compress.GzipBytes,
		StaticMaxAge:    3600,
	}
}

func benchmarkPreload(b *testing.B, cfg cache.PreloadConfig, prewarm bool) {
	root := preloadBenchRoot(b)
	b.ReportAllocs()
	for b.Loop() {
		c := cache.NewCache(256 << 20)
		st := c.Preload(root, cfg)
		if st.Files != 2000 {
			b.Fatalf("preloaded %d files, want 2000", st.Files)
		}
		if prewarm {
			pc := security.NewPathCache(0)
			pc.PreWarm(st.Paths, root, cfg.BlockDotfiles)
		}
	}
}

func BenchmarkPreload(b *testing.B) {
	b.Run("gzip", func(b *testing.B) { benchmarkPreload(b, gzipOnlyPreloadConfig(), false) })
	b.Run("gzip+prewarm", func(b *testing.B) { benchmarkPreload(b, gzipOnlyPreloadConfig(), true) })
}
