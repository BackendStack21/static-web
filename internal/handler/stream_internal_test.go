package handler

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// stClock is a manual clock.
type stClock struct{ t time.Time }

func (c *stClock) now() time.Time { return c.t }

// stSlowWriter accepts chunks through ReadFrom, as fasthttp's body writer
// does, and advances the clock as if the client took perByte per byte.
type stSlowWriter struct {
	clock   *stClock
	perByte time.Duration
	buf     bytes.Buffer
}

func (w *stSlowWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := w.buf.ReadFrom(r)
	w.clock.t = w.clock.t.Add(time.Duration(n) * w.perByte)
	return n, err
}

func (w *stSlowWriter) Write(p []byte) (int, error) { panic("stream must use ReadFrom") }

// stDeadlines records the deadlines a stream sets.
type stDeadlines struct{ got []time.Time }

func (d *stDeadlines) set(t time.Time) error { d.got = append(d.got, t); return nil }

func stContent(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// stStream returns a stream of content backed by a temporary file.
func stStream(t *testing.T, content []byte, wt time.Duration, clock *stClock, d *stDeadlines) *fileStream {
	t.Helper()
	p := filepath.Join(t.TempDir(), "body")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return newFileStream(f, io.NewSectionReader(f, 0, int64(len(content))), int64(len(content)), wt, clock.now, d.set)
}

// rateBytesPerSec is the per-byte delay of a client reading at rate.
func stPerByte(rate float64) time.Duration { return time.Duration(float64(time.Second) / rate) }

func TestStreamChunkSize_IsHalfAWriteTimeoutAtMinRate(t *testing.T) {
	for _, tc := range []struct {
		wt   time.Duration
		want int64
	}{
		{10 * time.Second, streamMinRate * 5},
		{4 * time.Second, streamMinRate * 2},
		{100 * time.Millisecond, streamMinChunk}, // clamped up
		{time.Hour, streamMaxChunk},              // clamped down
		{0, streamMaxChunk},                      // no deadlines
	} {
		if got := streamChunkSize(tc.wt); got != tc.want {
			t.Errorf("streamChunkSize(%v) = %d, want %d", tc.wt, got, tc.want)
		}
	}
}

// HTTP-F1 progress deadline: before every chunk the deadline moves to
// now + write_timeout, so a steady client is never cut off.
func TestFileStream_ExtendsDeadlineBeforeEachChunk(t *testing.T) {
	const wt = 10 * time.Second
	chunk := streamChunkSize(wt)
	content := stContent(int(3*chunk + chunk/2))
	clock := &stClock{t: time.Unix(1000, 0)}
	var d stDeadlines
	s := stStream(t, content, wt, clock, &d)
	w := &stSlowWriter{clock: clock, perByte: stPerByte(10 * streamMinRate)}

	start := clock.t
	n, err := s.WriteTo(w)
	if err != nil || n != int64(len(content)) || !bytes.Equal(w.buf.Bytes(), content) {
		t.Fatalf("WriteTo = %d, %v; body intact %v", n, err, bytes.Equal(w.buf.Bytes(), content))
	}
	if len(d.got) != 4 {
		t.Fatalf("deadlines set %d times, want 4 (one per chunk)", len(d.got))
	}
	chunkTime := time.Duration(chunk) * w.perByte
	for i, got := range d.got {
		if want := start.Add(time.Duration(i)*chunkTime + wt); !got.Equal(want) {
			t.Errorf("deadline %d = +%v, want +%v (chunk start + write_timeout)", i, got.Sub(start), want.Sub(start))
		}
	}
}

// A client at exactly the minimum rate completes, with each chunk taking
// half the write timeout.
func TestFileStream_ReaderAtMinRateCompletes(t *testing.T) {
	const wt = 10 * time.Second
	content := stContent(int(20 * streamChunkSize(wt)))
	clock := &stClock{t: time.Unix(1000, 0)}
	var d stDeadlines
	s := stStream(t, content, wt, clock, &d)
	w := &stSlowWriter{clock: clock, perByte: stPerByte(streamMinRate)}
	if n, err := s.WriteTo(w); err != nil || n != int64(len(content)) {
		t.Fatalf("WriteTo at the minimum rate = %d, %v; want the whole body", n, err)
	}
}

// HTTP-F1 rate cap: a client that completes every chunk well within the
// write timeout, but averages less than streamMinRate, is cut off once
// start + write_timeout + size/streamMinRate has passed; deadlines never
// extend past that point.
func TestFileStream_ReaderBelowMinRateIsCutByRateCap(t *testing.T) {
	const wt = 10 * time.Second
	chunk := streamChunkSize(wt)
	content := stContent(int(20 * chunk))
	clock := &stClock{t: time.Unix(1000, 0)}
	var d stDeadlines
	s := stStream(t, content, wt, clock, &d)
	// 3/4 of the minimum rate: a chunk takes 2/3 of the write timeout.
	w := &stSlowWriter{clock: clock, perByte: stPerByte(0.75 * streamMinRate)}
	if chunkTime := time.Duration(chunk) * w.perByte; chunkTime >= wt {
		t.Fatalf("test setup: a chunk takes %v, not under the write timeout %v", chunkTime, wt)
	}

	start := clock.t
	n, err := s.WriteTo(w)
	if !errors.Is(err, errStreamTooSlow) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("WriteTo error = %v, want errStreamTooSlow (an os.ErrDeadlineExceeded)", err)
	}
	if n >= int64(len(content)) || n%chunk != 0 {
		t.Errorf("wrote %d of %d bytes before the cut, want whole chunks and less than the body", n, len(content))
	}
	capAt := streamRateCap(start, int64(len(content)), wt)
	if clock.t.Before(capAt) {
		t.Errorf("cut at +%v, before the rate cap +%v", clock.t.Sub(start), capAt.Sub(start))
	}
	for i, got := range d.got {
		if got.After(capAt) {
			t.Errorf("deadline %d = +%v is past the rate cap +%v", i, got.Sub(start), capAt.Sub(start))
		}
	}
	if last := d.got[len(d.got)-1]; !last.Equal(capAt) {
		t.Errorf("last deadline = +%v, want it clamped to the rate cap +%v", last.Sub(start), capAt.Sub(start))
	}
}

func TestFileStream_NoWriteTimeoutSetsNoDeadline(t *testing.T) {
	content := stContent(3 * streamMinChunk)
	clock := &stClock{t: time.Unix(1000, 0)}
	var d stDeadlines
	s := stStream(t, content, 0, clock, &d)
	w := &stSlowWriter{clock: clock, perByte: time.Second} // absurdly slow
	if n, err := s.WriteTo(w); err != nil || n != int64(len(content)) {
		t.Fatalf("WriteTo = %d, %v", n, err)
	}
	if len(d.got) != 0 {
		t.Errorf("set %d deadlines with write_timeout 0, want none", len(d.got))
	}
}

// Read gives the same bytes and extends the deadline at the same chunk
// boundaries as WriteTo.
func TestFileStream_ReadMatchesWriteTo(t *testing.T) {
	const wt = 2 * time.Second
	chunk := streamChunkSize(wt)
	content := stContent(int(2*chunk + 100))
	clock := &stClock{t: time.Unix(1000, 0)}
	var d stDeadlines
	s := stStream(t, content, wt, clock, &d)
	got, err := io.ReadAll(struct{ io.Reader }{s}) // hide WriteTo
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("ReadAll = %d bytes, %v; body intact %v", len(got), err, bytes.Equal(got, content))
	}
	if len(d.got) != 3 {
		t.Errorf("deadlines set %d times, want 3 (one per chunk)", len(d.got))
	}
}

// stServeLarge runs a GET for a 1 MiB large file through h and returns the
// response's stream and the file it owns.
func stServeLarge(t *testing.T) (*fasthttp.RequestCtx, *fileStream, *os.File, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "big.bin")
	if err := os.WriteFile(p, stContent(1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Cache.MaxFileSize = 1024
	cfg.Server.WriteTimeout = 10 * time.Second
	h := NewFileHandler(cfg, nil)
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/big.bin")
	h.HandleRequest(&ctx)
	s, ok := ctx.Response.BodyStream().(*fileStream)
	if !ok {
		t.Fatalf("body stream is %T, want *fileStream", ctx.Response.BodyStream())
	}
	return &ctx, s, s.f, p
}

type stFailingWriter struct{ after int }

func (w *stFailingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	n := min(len(p), w.after)
	w.after -= n
	return n, nil
}

func stAssertClosed(t *testing.T, label string, s *fileStream, f *os.File) {
	t.Helper()
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("%s: file still open (Stat error %v)", label, err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("%s: second Close = %v, want nil", label, err)
	}
}

// fasthttp closes the stream, and so the file, exactly once on every path.
func TestFileStream_FileClosedOnEveryPath(t *testing.T) {
	t.Run("body written", func(t *testing.T) {
		ctx, s, f, _ := stServeLarge(t)
		if got := len(ctx.Response.Body()); got != 1<<20 {
			t.Fatalf("body = %d bytes", got)
		}
		stAssertClosed(t, "written", s, f)
	})
	t.Run("client aborts", func(t *testing.T) {
		ctx, s, f, _ := stServeLarge(t)
		if err := ctx.Response.BodyWriteTo(&stFailingWriter{after: 100 << 10}); err == nil {
			t.Fatal("BodyWriteTo succeeded, want the write error")
		}
		stAssertClosed(t, "aborted", s, f)
	})
	t.Run("file shrinks", func(t *testing.T) {
		ctx, s, f, p := stServeLarge(t)
		if err := os.Truncate(p, 1000); err != nil {
			t.Fatal(err)
		}
		var sink bytes.Buffer
		if err := ctx.Response.BodyWriteTo(&sink); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("BodyWriteTo error = %v, want io.ErrUnexpectedEOF", err)
		}
		stAssertClosed(t, "shrunk", s, f)
	})
	t.Run("response reset before writing", func(t *testing.T) {
		ctx, s, f, _ := stServeLarge(t)
		ctx.Response.Reset()
		stAssertClosed(t, "reset", s, f)
	})
	t.Run("error after the stream was set", func(t *testing.T) {
		ctx, s, f, _ := stServeLarge(t)
		writeError(ctx, "Internal Server Error", fasthttp.StatusInternalServerError)
		stAssertClosed(t, "replaced", s, f)
	})
}

// On a real connection the rate cap ends the response: the client gets the
// chunks sent before the cap passed and then the connection closes. A fake
// clock jumps past the cap after the first chunk.
func TestLargeFile_RateCapClosesConnectionOnWire(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const size = 4 << 20
	if err := os.WriteFile(filepath.Join(root, "big.bin"), stContent(size), 0o644); err != nil {
		t.Fatal(err)
	}
	const wt = time.Second
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Cache.MaxFileSize = 1024
	cfg.Server.WriteTimeout = wt
	h := NewFileHandler(cfg, nil)
	t0 := time.Now()
	var calls atomic.Int64
	h.clock = func() time.Time {
		if calls.Add(1) == 1 {
			return t0
		}
		return t0.Add(time.Hour) // past the cap for every later chunk
	}

	srv := &fasthttp.Server{Handler: h.HandleRequest, WriteTimeout: wt}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = srv.Shutdown() })

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "GET /big.bin HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err == nil || n != streamChunkSize(wt) {
		t.Errorf("received %d bytes then %v; want exactly one chunk (%d bytes) and a cut connection",
			n, err, streamChunkSize(wt))
	}
}
