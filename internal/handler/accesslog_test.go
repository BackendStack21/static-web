package handler

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// recordingWriter is a goroutine-safe io.Writer that records every Write.
type recordingWriter struct {
	mu     sync.Mutex
	writes int
	buf    bytes.Buffer
	fail   int // fail this many writes first
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail > 0 {
		w.fail--
		return 0, errors.New("disk full")
	}
	w.writes++
	return w.buf.Write(p)
}

func (w *recordingWriter) snapshot() (writes int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes, w.buf.String()
}

func (w *recordingWriter) lines() []string {
	_, text := w.snapshot()
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// withLogOutput points the standard logger at w for the test.
func withLogOutput(t *testing.T, w *recordingWriter) {
	t.Helper()
	prev := log.Writer()
	log.SetOutput(w)
	t.Cleanup(func() {
		_ = FlushAccessLog()
		log.SetOutput(prev)
	})
}

func accessLogCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 1 << 20
	cfg.Cache.MaxFileSize = 1 << 20
	cfg.Security.BlockDotfiles = true
	return cfg
}

func serve(h fasthttp.RequestHandler, method, uri string) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	h(&ctx)
	return &ctx
}

// P-F10: one write(2) per request under the log mutex cost +80% serial and
// 3.5x at 10 cores. Access-log lines must be buffered and written in batches.
func TestAccessLog_RequestsAreBufferedNotWrittenOneByOne(t *testing.T) {
	dst := &recordingWriter{}
	withLogOutput(t, dst)
	h := BuildHandler(accessLogCfg(t), nil)

	const n = 50
	for i := 0; i < n; i++ {
		serve(h, "GET", "/a.txt")
	}
	if writes, _ := dst.snapshot(); writes != 0 {
		t.Errorf("%d requests caused %d writes to the log destination before a flush, want 0 (buffered)", n, writes)
	}

	if err := FlushAccessLog(); err != nil {
		t.Fatalf("FlushAccessLog: %v", err)
	}
	writes, _ := dst.snapshot()
	if writes != 1 {
		t.Errorf("FlushAccessLog issued %d writes, want 1 batched write", writes)
	}
	if got := len(dst.lines()); got != n {
		t.Errorf("got %d access-log lines after flush, want %d", got, n)
	}
}

func TestAccessLog_LineFormatIsUnchanged(t *testing.T) {
	dst := &recordingWriter{}
	withLogOutput(t, dst)
	h := BuildHandler(accessLogCfg(t), nil)
	serve(h, "GET", "/a.txt?x=1")
	if err := FlushAccessLog(); err != nil {
		t.Fatal(err)
	}
	lines := dst.lines()
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want 1", lines)
	}
	// Standard logger prefix and flags (date and time), then the line.
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(log.Prefix()) + `\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} GET /a\.txt\?x=1 200 5 \S+$`)
	if !re.MatchString(lines[0]) {
		t.Errorf("access-log line = %q, want it to match %s", lines[0], re)
	}
}

func TestAccessLog_FlushedByTimerWithoutExplicitFlush(t *testing.T) {
	dst := &recordingWriter{}
	withLogOutput(t, dst)
	h := BuildHandler(accessLogCfg(t), nil)
	serve(h, "GET", "/a.txt")

	deadline := time.Now().Add(accessLogFlushInterval + 3*time.Second)
	for time.Now().Before(deadline) {
		if len(dst.lines()) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("access-log line not written within %v of the request", accessLogFlushInterval+3*time.Second)
}

func TestAccessLog_FullBufferIsWrittenWithoutWaitingForTheTimer(t *testing.T) {
	dst := &recordingWriter{}
	w := newAsyncLogWriter(dst, time.Hour, 1024)
	logger := log.New(w, "", 0)
	line := strings.Repeat("x", 99) // 100 bytes with the newline
	for i := 0; i < 25; i++ {
		logger.Print(line)
	}
	writes, text := dst.snapshot()
	if writes == 0 {
		t.Fatal("2.5 KB of lines through a 1 KB buffer caused no write before the timer")
	}
	if len(text) > 25*100 || len(text)%100 != 0 {
		t.Fatalf("partial line written: %d bytes", len(text))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := len(dst.lines()); got != 25 {
		t.Errorf("got %d lines after flush, want 25", got)
	}
}

func TestAccessLog_FlushAccessLogCoversEveryHandler(t *testing.T) {
	a, b := &recordingWriter{}, &recordingWriter{}
	withLogOutput(t, a)
	ha := BuildHandler(accessLogCfg(t), nil)
	log.SetOutput(b)
	hb := BuildHandler(accessLogCfg(t), nil)

	serve(ha, "GET", "/a.txt")
	serve(hb, "GET", "/a.txt")
	serve(hb, "HEAD", "/a.txt")
	if err := FlushAccessLog(); err != nil {
		t.Fatal(err)
	}
	if got := len(a.lines()); got != 1 {
		t.Errorf("first handler's destination got %d lines, want 1", got)
	}
	if got := len(b.lines()); got != 2 {
		t.Errorf("second handler's destination got %d lines, want 2", got)
	}
}

func TestAccessLog_WriteErrorDoesNotLoseLaterLines(t *testing.T) {
	dst := &recordingWriter{fail: 1}
	w := newAsyncLogWriter(dst, time.Hour, 1<<10)
	logger := log.New(w, "", 0)
	logger.Print("lost")
	if err := w.Flush(); err == nil {
		t.Error("Flush returned nil, want the destination's write error")
	}
	logger.Print("kept")
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush after a failed write: %v", err)
	}
	if got := dst.lines(); len(got) != 1 || got[0] != "kept" {
		t.Errorf("lines after a failed write = %q, want [kept]", got)
	}
}

func TestAccessLog_ConcurrentRequestsLoseNoLines(t *testing.T) {
	dst := &recordingWriter{}
	withLogOutput(t, dst)
	h := BuildHandler(accessLogCfg(t), nil)

	const workers, per = 8, 300
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				serve(h, "GET", fmt.Sprintf("/a.txt?g=%d&i=%d", g, i))
			}
		}(g)
	}
	wg.Wait()
	if err := FlushAccessLog(); err != nil {
		t.Fatal(err)
	}
	lines := dst.lines()
	if len(lines) != workers*per {
		t.Fatalf("got %d lines, want %d", len(lines), workers*per)
	}
	seen := make(map[string]bool, len(lines))
	re := regexp.MustCompile(`GET (/a\.txt\?g=\d+&i=\d+) 200 `)
	for _, l := range lines {
		m := re.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("malformed or interleaved line %q", l)
		}
		seen[m[1]] = true
	}
	if len(seen) != workers*per {
		t.Errorf("got %d distinct requests logged, want %d", len(seen), workers*per)
	}
}
