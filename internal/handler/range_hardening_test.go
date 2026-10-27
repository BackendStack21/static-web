package handler_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// TestRangeHardening_B1 pins the multi-range amplification fix (RFC 9110 §14.2, §15.5.17).
func TestRangeHardening_B1(t *testing.T) {
	root, cfg := setupTestDir(t)
	const size = 100 * 1024
	content := strings.Repeat("a", size)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg.Compression.Enabled = false

	do := func(t *testing.T, rangeHdr string) *fasthttp.RequestCtx {
		t.Helper()
		c := cache.NewCache(cfg.Cache.MaxBytes)
		h := handler.BuildHandler(cfg, c)
		ctx := newTestCtx("GET", "/big.txt")
		ctx.Request.Header.Set("Range", rangeHdr)
		h(ctx)
		return ctx
	}

	t.Run("overlapping range flood does not amplify", func(t *testing.T) {
		hdr := "bytes=" + strings.Repeat("0-,", 1300) + "0-"
		ctx := do(t, hdr)
		if got := len(ctx.Response.Body()); got > 2*size {
			t.Errorf("body = %d bytes for a %d byte file (status %d); want <= %d", got, size, ctx.Response.StatusCode(), 2*size)
		}
	})

	t.Run("summed ranges larger than file serve 200 full", func(t *testing.T) {
		ctx := do(t, "bytes=0-,0-")
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Errorf("status = %d, want 200", ctx.Response.StatusCode())
		}
		if got := len(ctx.Response.Body()); got != size {
			t.Errorf("body = %d, want %d", got, size)
		}
	})

	t.Run("unsatisfiable range returns 416", func(t *testing.T) {
		ctx := do(t, "bytes="+strconv.Itoa(size+10)+"-")
		if ctx.Response.StatusCode() != fasthttp.StatusRequestedRangeNotSatisfiable {
			t.Errorf("status = %d, want 416", ctx.Response.StatusCode())
		}
		want := "bytes */" + strconv.Itoa(size)
		if got := string(ctx.Response.Header.Peek("Content-Range")); got != want {
			t.Errorf("Content-Range = %q, want %q", got, want)
		}
	})
}
