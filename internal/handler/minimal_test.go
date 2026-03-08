package handler_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
)

func TestBuildBenchmarkHandlerServesIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>ok</h1>"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"

	h := handler.BuildBenchmarkHandler(cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "<h1>ok</h1>" {
		t.Fatalf("body = %q, want %q", got, "<h1>ok</h1>")
	}
	// Benchmark handler should not set X-Cache.
	if got := rr.Header().Get("X-Cache"); got != "" {
		t.Fatalf("X-Cache = %q, want empty", got)
	}
}

func TestBuildBenchmarkHandlerServesNamedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "style.css"), []byte("body{}"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root

	h := handler.BuildBenchmarkHandler(cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "body{}" {
		t.Fatalf("body = %q, want %q", got, "body{}")
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/css", ct)
	}
}

func TestBuildBenchmarkHandlerRejectsPost(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root

	h := handler.BuildBenchmarkHandler(cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestBuildBenchmarkHandlerHidesMissingEscapeTargets(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Files.Root = root

	h := handler.BuildBenchmarkHandler(cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/../../does-not-exist", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestBuildBenchmarkHandlerReturns404ForMissingFile(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Files.Root = root

	h := handler.BuildBenchmarkHandler(cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nope.txt", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestBuildBenchmarkHandlerHEADOmitsBody(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>ok</h1>"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"

	h := handler.BuildBenchmarkHandler(cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD body should be empty, got %d bytes", rr.Body.Len())
	}
	if cl := rr.Header().Get("Content-Length"); cl != "11" {
		t.Fatalf("Content-Length = %q, want 11", cl)
	}
}

func TestBuildBenchmarkHandlerSubdirIndex(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "docs")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "index.html"), []byte("docs"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"

	h := handler.BuildBenchmarkHandler(cfg)

	// Request /docs should resolve to /docs/index.html.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "docs" {
		t.Fatalf("body = %q, want %q", got, "docs")
	}
}

func BenchmarkBenchmarkHandler(b *testing.B) {
	root := b.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>bench</h1>"), 0644); err != nil {
		b.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"

	h := handler.BuildBenchmarkHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
	}
}
