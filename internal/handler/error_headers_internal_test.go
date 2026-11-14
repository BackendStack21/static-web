package handler

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// ehRegularInfo is an os.FileInfo describing a small regular file.
type ehRegularInfo struct{ size int64 }

func (i ehRegularInfo) Name() string       { return "f" }
func (i ehRegularInfo) Size() int64        { return i.size }
func (i ehRegularInfo) Mode() fs.FileMode  { return 0o644 }
func (i ehRegularInfo) ModTime() time.Time { return time.Unix(1, 0) }
func (i ehRegularInfo) IsDir() bool        { return false }
func (i ehRegularInfo) Sys() any           { return nil }

func ehHandler(t *testing.T) (*FileHandler, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Cache.MaxFileSize = 1 << 20
	cfg.Security.BlockDotfiles = true
	return NewFileHandler(cfg, nil), root
}

// ehCtx returns a request whose response already carries the headers outer
// middleware sets before the file handler runs.
func ehCtx(uri string) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI(uri)
	for k, v := range headersThatMustSurviveAnError {
		ctx.Response.Header.Set(k, v)
	}
	return &ctx
}

func ehAssertKept(t *testing.T, label string, ctx *fasthttp.RequestCtx, wantStatus int) {
	t.Helper()
	if got := ctx.Response.StatusCode(); got != wantStatus {
		t.Errorf("%s: status = %d, want %d", label, got, wantStatus)
	}
	for k, want := range headersThatMustSurviveAnError {
		got := string(ctx.Response.Header.Peek(k))
		if k == "Vary" {
			// Other tokens may have been merged in; Origin must survive.
			if !strings.Contains(got, want) {
				t.Errorf("%s: Vary = %q, want it to keep %q", label, got, want)
			}
			continue
		}
		if got != want {
			t.Errorf("%s: %s = %q, want %q (kept from outer middleware)", label, k, got, want)
		}
	}
}

// HTTP-F4: every error the file handler writes itself keeps the security,
// CORS and HSTS headers outer middleware set (ctx.Error dropped them).
func TestFileHandlerErrors_KeepOuterHeaders(t *testing.T) {
	h, root := ehHandler(t)
	dir := filepath.Join(root, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("500 on a stat error", func(t *testing.T) {
		ctx := ehCtx("/x")
		h.serveFromDisk(ctx, filepath.Join(root, "x"), "/x", nil, errors.New("i/o error"))
		ehAssertKept(t, "stat error", ctx, fasthttp.StatusInternalServerError)
	})
	t.Run("403 on a permission stat error", func(t *testing.T) {
		ctx := ehCtx("/x")
		h.serveFromDisk(ctx, filepath.Join(root, "x"), "/x", nil, fs.ErrPermission)
		ehAssertKept(t, "stat permission", ctx, fasthttp.StatusForbidden)
	})
	t.Run("500 on a read error", func(t *testing.T) {
		// Reading a directory as a file fails with EISDIR.
		ctx := ehCtx("/d")
		h.serveFromDisk(ctx, dir, "/d", ehRegularInfo{size: 10}, nil)
		ehAssertKept(t, "read error", ctx, fasthttp.StatusInternalServerError)
	})
	t.Run("500 on a large-file open error", func(t *testing.T) {
		ctx := ehCtx("/gone.bin")
		h.serveLargeFile(ctx, filepath.Join(root, "gone.bin"), "/gone.bin")
		ehAssertKept(t, "large open error", ctx, fasthttp.StatusInternalServerError)
	})
	t.Run("403 on a lexical security error", func(t *testing.T) {
		ctx := ehCtx("/.env")
		h.HandleRequest(ctx)
		ehAssertKept(t, "dotfile", ctx, fasthttp.StatusForbidden)
	})
}
