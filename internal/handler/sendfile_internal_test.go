package handler

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// sfHandler serves a root with big.bin (size bytes of stContent) as a large
// file.
func sfHandler(t *testing.T, size int) (*FileHandler, []byte) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := stContent(size)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Cache.MaxFileSize = 1024
	cfg.Server.WriteTimeout = 10 * time.Second
	return NewFileHandler(cfg, nil), content
}

// P-F6: fasthttp only uses sendfile(2) when the body it copies is an
// *os.File, possibly inside an *io.LimitedReader. fileStream.WriteTo offers
// each chunk as a LimitedReader over its source, so full and single-range
// bodies must use the file itself (positioned at the first byte) as the
// source, not a wrapper such as an io.SectionReader.
func TestLargeFile_FullAndSingleRangeStreamTheFileItself(t *testing.T) {
	h, _ := sfHandler(t, 1<<20)
	for _, rng := range []string{"", "bytes=1000-1999", "bytes=-500"} {
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod("GET")
		ctx.Request.SetRequestURI("/big.bin")
		if rng != "" {
			ctx.Request.Header.Set("Range", rng)
		}
		h.HandleRequest(&ctx)
		s, ok := ctx.Response.BodyStream().(*fileStream)
		if !ok {
			t.Fatalf("Range %q: body stream is %T, want *fileStream", rng, ctx.Response.BodyStream())
		}
		if _, ok := s.src.(*os.File); !ok {
			t.Errorf("Range %q: stream source is %T, want *os.File so that fasthttp can sendfile", rng, s.src)
		}
		ctx.Response.Reset()
	}
}

// Bodies sent with sendfile must be the right bytes on the wire: the full
// file, a single range (the file positioned at its start) and multipart
// ranges, one after another on one keep-alive connection.
func TestLargeFile_BodiesAreCorrectOnWire(t *testing.T) {
	const size = 3<<20 + 12345
	h, content := sfHandler(t, size)
	srv := &fasthttp.Server{Handler: h.HandleRequest, WriteTimeout: 10 * time.Second}
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
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(c)
	get := func(rng string) *http.Response {
		t.Helper()
		req := "GET /big.bin HTTP/1.1\r\nHost: x\r\n"
		if rng != "" {
			req += "Range: " + rng + "\r\n"
		}
		fmt.Fprint(c, req+"\r\n")
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("Range %q: %v", rng, err)
		}
		return resp
	}
	readAll := func(resp *http.Response) []byte {
		t.Helper()
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return b
	}

	if b := readAll(get("")); !bytes.Equal(b, content) {
		t.Errorf("full body: %d bytes, equal %v", len(b), bytes.Equal(b, content))
	}
	for _, r := range []struct {
		hdr        string
		start, end int
	}{
		{"bytes=1000-1999", 1000, 2000},
		{"bytes=1048570-2097160", 1048570, 2097161},
		{"bytes=-777", size - 777, size},
		{"bytes=0-0", 0, 1},
	} {
		resp := get(r.hdr)
		if resp.StatusCode != http.StatusPartialContent {
			t.Errorf("%s: status %d", r.hdr, resp.StatusCode)
		}
		if b := readAll(resp); !bytes.Equal(b, content[r.start:r.end]) {
			t.Errorf("%s: body is not bytes %d-%d (%d bytes)", r.hdr, r.start, r.end-1, len(b))
		}
	}

	resp := get("bytes=0-9,2000000-2000099")
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("multipart Content-Type: %v", err)
	}
	mr := multipart.NewReader(bytes.NewReader(readAll(resp)), params["boundary"])
	for _, want := range [][]byte{content[0:10], content[2000000:2000100]} {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		if b, _ := io.ReadAll(part); !bytes.Equal(b, want) {
			t.Errorf("multipart part = %d bytes, not the requested range", len(b))
		}
	}

	if b := readAll(get("")); !bytes.Equal(b, content) {
		t.Errorf("full body after ranges: %d bytes, equal %v", len(b), bytes.Equal(b, content))
	}
}
