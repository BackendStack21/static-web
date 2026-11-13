//go:build unix

package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

// HTTP-F1: the progress deadline must still cut off a client that stops
// reading. It is extended only when a chunk of the body has been written,
// so the server gives up about write_timeout after the client stalls,
// however much of the download is left.
func TestLargeFile_StalledReaderIsCutOff(t *testing.T) {
	const (
		size         = 64 << 20
		writeTimeout = 300 * time.Millisecond
	)
	s := lfdStart(t, size, writeTimeout, false)

	// A small receive buffer keeps the data in flight well below the body
	// size, so the server blocks soon after the client stops reading.
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 16<<10)
		}); err != nil {
			return err
		}
		return serr
	}}
	c, err := d.DialContext(context.Background(), "tcp", s.HTTPAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReaderSize(c, 16<<10)

	fmt.Fprintf(c, "GET /big.bin HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength != size {
		t.Fatalf("status %d, Content-Length %d", resp.StatusCode, resp.ContentLength)
	}
	head := make([]byte, 64<<10)
	if _, err := io.ReadFull(resp.Body, head); err != nil {
		t.Fatalf("read the start of the body: %v", err)
	}

	time.Sleep(6 * writeTimeout) // stall

	// Drain what the kernel buffered before the server gave up.
	drainStart := time.Now()
	n, err := io.Copy(io.Discard, resp.Body)
	got := int64(len(head)) + n
	if err == nil || got >= size {
		t.Fatalf("stalled client still received %d of %d bytes (err %v); want the server to cut it off", got, size, err)
	}
	if d := time.Since(drainStart); d > 5*time.Second {
		t.Errorf("drain after the stall took %v; the server did not close the connection", d)
	}
	t.Logf("stalled client received %d of %d bytes, then %v", got, size, err)
}
