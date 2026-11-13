package server

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// alSyncBuffer is a goroutine-safe bytes.Buffer for capturing log output.
type alSyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *alSyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *alSyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var alAccessLine = regexp.MustCompile(`(?m) GET /a\.txt\?n=\d+ 200 5 `)

// alStartLoggedServer starts a server whose handler writes access logs into
// the returned buffer.
func alStartLoggedServer(t *testing.T) (*Server, *config.Config, *alSyncBuffer) {
	t.Helper()
	logs := &alSyncBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Security.BlockDotfiles = true
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.ReadTimeout = 5 * time.Second
	cfg.Server.WriteTimeout = 5 * time.Second
	cfg.Server.IdleTimeout = 5 * time.Second
	cfg.Server.ShutdownTimeout = 5 * time.Second

	s := New(&cfg.Server, &cfg.Security, handler.BuildHandler(cfg, nil))
	if err := s.Start(&cfg.Server); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s, cfg, logs
}

func alRequestN(t *testing.T, s *Server, n int) {
	t.Helper()
	client := &fasthttp.Client{}
	for i := 0; i < n; i++ {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.SetRequestURI("http://" + s.HTTPAddr().String() + "/a.txt?n=" + string(rune('0'+i)))
		if err := client.DoTimeout(req, resp, 5*time.Second); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode())
		}
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
	client.CloseIdleConnections()
}

// P-F10: access logs are buffered; a graceful shutdown must write them out
// before returning so that the process can exit without losing lines.
func TestShutdown_FlushesBufferedAccessLog(t *testing.T) {
	s, _, logs := alStartLoggedServer(t)
	alRequestN(t, s, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := len(alAccessLine.FindAllString(logs.String(), -1)); got != 3 {
		t.Errorf("after Shutdown returned, %d access-log lines were written, want 3; log:\n%s", got, logs.String())
	}
}

func TestRunSignalHandler_SIGTERMFlushesBufferedAccessLog(t *testing.T) {
	// Keep SIGTERM from killing the test binary should it arrive before
	// RunSignalHandler has registered for it.
	guard := make(chan os.Signal, 16)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	s, cfg, logs := alStartLoggedServer(t)
	alRequestN(t, s, 4)

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunSignalHandler(context.Background(), s, nil, nil, &cfg)
	}()
	deadline := time.After(10 * time.Second)
	for stopped := false; !stopped; {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatalf("kill: %v", err)
		}
		select {
		case <-done:
			stopped = true
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatal("RunSignalHandler did not return after SIGTERM")
		}
	}
	if got := len(alAccessLine.FindAllString(logs.String(), -1)); got != 4 {
		t.Errorf("after SIGTERM shutdown, %d access-log lines were written, want 4; log:\n%s", got, logs.String())
	}
}
