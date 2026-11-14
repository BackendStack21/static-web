package server

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
)

// lfdStart serves a root holding big.bin (size bytes, sparse) and small.txt
// through the real handler chain and server, with the given write timeout.
func lfdStart(t *testing.T, size int64, writeTimeout time.Duration, withTLS bool) *Server {
	t.Helper()
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Files.MaxServeFileSize = 1 << 30
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 16 << 20
	cfg.Cache.MaxFileSize = 1 << 20
	cfg.Security.BlockDotfiles = true
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.ReadTimeout = 5 * time.Second
	cfg.Server.WriteTimeout = writeTimeout
	cfg.Server.IdleTimeout = 10 * time.Second
	if withTLS {
		cfg.Server.TLSAddr = "127.0.0.1:0"
		cfg.Server.RedirectHost = "localhost"
		cfg.Server.TLSCert, cfg.Server.TLSKey = writeSelfSignedCert(t, t.TempDir())
	}

	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	s := New(&cfg.Server, &cfg.Security, h)
	if err := s.Start(&cfg.Server); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { shutdownServer(t, s) })
	return s
}

// lfdDial connects to the HTTP listener, or the HTTPS one when withTLS.
func lfdDial(t *testing.T, s *Server, withTLS bool) net.Conn {
	t.Helper()
	var c net.Conn
	var err error
	if withTLS {
		c, err = tls.Dial("tcp", s.HTTPSAddr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test-only self-signed cert
	} else {
		c, err = net.Dial("tcp", s.HTTPAddr().String())
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// lfdSlowRead reads body in 1 MiB steps with a pause after each, like a
// client on a link slower than the server. It returns the bytes read and
// the error that ended the read: io.EOF at the end of a body whose size is
// a multiple of 1 MiB, io.ErrUnexpectedEOF (or another error) if cut short.
func lfdSlowRead(body io.Reader, pause time.Duration) (int64, error) {
	buf := make([]byte, 1<<20)
	var got int64
	for {
		n, err := io.ReadFull(body, buf)
		got += int64(n)
		if err != nil {
			return got, err
		}
		time.Sleep(pause)
	}
}

// HTTP-F1: fasthttp applies write_timeout once per response unless the
// handler sets its own deadline, so it was a cap on the whole download: a
// client receiving 20 MiB/s, steadily, lost the connection write_timeout
// after the response started. Large-file bodies now extend the deadline as
// they make progress. The connection stays usable afterwards.
func TestLargeFile_SlowReaderCompletesPastWriteTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		withTLS bool
		size    int64
	}{
		{"http", false, 32 << 20},
		{"https", true, 16 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const writeTimeout = 400 * time.Millisecond
			s := lfdStart(t, tc.size, writeTimeout, tc.withTLS)
			c := lfdDial(t, s, tc.withTLS)
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			br := bufio.NewReaderSize(c, 64<<10)

			fmt.Fprintf(c, "GET /big.bin HTTP/1.1\r\nHost: localhost\r\n\r\n")
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			start := time.Now()
			got, err := lfdSlowRead(resp.Body, 50*time.Millisecond)
			elapsed := time.Since(start)
			resp.Body.Close()
			if got != tc.size || !errors.Is(err, io.EOF) {
				t.Fatalf("read %d of %d bytes in %v (write_timeout %v), ended with %v",
					got, tc.size, elapsed.Round(time.Millisecond), writeTimeout, err)
			}
			if elapsed < 2*writeTimeout {
				t.Logf("download took %v, not much longer than write_timeout %v; the test may not exercise HTTP-F1", elapsed, writeTimeout)
			}

			// The same keep-alive connection serves the next request.
			fmt.Fprintf(c, "GET /small.txt HTTP/1.1\r\nHost: localhost\r\n\r\n")
			resp, err = http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("second request on the connection: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(body) != "small" {
				t.Errorf("second request: status %d body %q", resp.StatusCode, body)
			}
		})
	}
}
