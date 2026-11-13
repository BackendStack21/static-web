package handler

import (
	"fmt"
	"io"
	"os"
	"time"
)

// Large-file download deadlines (HTTP-F1).
//
// fasthttp applies Server.WriteTimeout once per response, as a deadline for
// the whole response, unless the handler sets its own. A large file sent
// more slowly than size/write_timeout was therefore cut off however steadily
// it was progressing. The body stream of a large-file response (fileStream)
// manages the connection's write deadline itself, from the configured
// write_timeout (wt):
//
//   - Progress deadline. The body is written in chunks of streamChunkSize(wt)
//     bytes, and before each chunk the deadline becomes now + wt. A client
//     that stops reading is cut off at most wt after the last chunk started.
//   - Rate cap. The deadline never passes start + wt + n/streamMinRate for an
//     n-byte body, where start is when the body began. A client that reads
//     just enough to keep completing chunks, but averages less than
//     streamMinRate, is cut off too instead of holding a goroutine and a file
//     descriptor for as long as it likes.
//
// A chunk is what a client at streamMinRate receives in wt/2, so such a
// client finishes every chunk with half the progress deadline to spare.
// Progress is measured when bytes enter the kernel's socket buffer, not when
// the client reads them, which is why the margin is needed. With wt <= 0 (no
// write timeout configured) no deadline is set at all.
const (
	// streamMinRate is the slowest average rate, in bytes per second, at
	// which a large-file download is kept open: 32 KiB/s, i.e. 256 kbit/s.
	// Real slow clients stay above it: the slowest common mobile profile
	// ("Slow 3G" in browser dev tools, 400 kbit/s) has 50% headroom. A
	// client below it would need more than 9 hours per GiB. And it makes a
	// slow-read attack cost real bandwidth: every connection an attacker
	// wants to hold open must actually receive 32 KiB/s.
	streamMinRate = 32 << 10
	// streamMinChunk and streamMaxChunk bound the chunk size. The minimum
	// keeps tiny write timeouts from turning the stream into many small
	// writes; with it, a write timeout under 4s requires more than
	// streamMinRate per progress window. The maximum bounds the chunk for
	// very long write timeouts, where the rate cap then does the work. At
	// the default 10s write timeout a chunk is 160 KiB, which costs
	// sendfile no measurable throughput.
	streamMinChunk = 64 << 10
	streamMaxChunk = 8 << 20
)

// errStreamTooSlow ends a large-file body whose rate cap has passed. It
// wraps os.ErrDeadlineExceeded because it is the same condition as the
// connection's write deadline expiring during a chunk ("i/o timeout").
var errStreamTooSlow = fmt.Errorf("large-file download fell below %d bytes/s: %w", streamMinRate, os.ErrDeadlineExceeded)

// streamChunkSize returns the number of body bytes sent per progress
// deadline for write timeout wt (see the constants above).
func streamChunkSize(wt time.Duration) int64 {
	if wt <= 0 {
		return streamMaxChunk
	}
	c := int64(float64(streamMinRate) * wt.Seconds() / 2)
	return min(max(c, streamMinChunk), streamMaxChunk)
}

// streamRateCap returns the latest deadline for an n-byte body whose first
// chunk started at start: start + wt + n/streamMinRate.
func streamRateCap(start time.Time, n int64, wt time.Duration) time.Time {
	// Whole seconds first, so that n*time.Second cannot overflow.
	d := time.Duration(n/streamMinRate)*time.Second +
		time.Duration(n%streamMinRate)*time.Second/streamMinRate
	return start.Add(wt + d)
}

// fileStream is the body stream of a large-file response. It implements
// fasthttp.BodyWriterTo, so fasthttp writes the body with WriteTo, and
// io.Closer, so fasthttp closes it exactly once: after the body is written,
// after a write error, or when the response is reset before being written.
//
// Before every chunk it extends the connection's write deadline as described
// at streamMinRate. Read applies the same chunking for other consumers.
type fileStream struct {
	f         *os.File  // closed by Close
	src       io.Reader // the body bytes, read from f
	remaining int64     // body bytes not yet produced
	size      int64     // body length, for the rate cap
	lr        io.LimitedReader

	// Deadline management; disabled when wt <= 0.
	wt          time.Duration
	chunk       int64
	budget      int64 // bytes left before the next deadline extension
	capAt       time.Time
	now         func() time.Time
	setDeadline func(time.Time) error
}

// newFileStream returns a stream of the size bytes of src, which reads from
// f. The stream owns f. setDeadline sets the connection's write deadline
// (fasthttp.RequestCtx.SetWriteDeadline, which stays valid while fasthttp
// writes the body after the handler has returned); now is the clock.
func newFileStream(f *os.File, src io.Reader, size int64, wt time.Duration, now func() time.Time, setDeadline func(time.Time) error) *fileStream {
	return &fileStream{
		f:           f,
		src:         src,
		remaining:   size,
		size:        size,
		wt:          wt,
		chunk:       streamChunkSize(wt),
		now:         now,
		setDeadline: setDeadline,
	}
}

// refresh starts a new chunk when the current one is used up: it extends the
// write deadline to now + wt, but not past the rate cap, which is fixed when
// the first chunk starts. It fails with errStreamTooSlow once the cap has
// passed.
func (s *fileStream) refresh() error {
	if s.budget > 0 {
		return nil
	}
	s.budget = s.chunk
	if s.wt <= 0 {
		return nil
	}
	now := s.now()
	if s.capAt.IsZero() {
		s.capAt = streamRateCap(now, s.size, s.wt)
	}
	if !now.Before(s.capAt) {
		return errStreamTooSlow
	}
	d := now.Add(s.wt)
	if d.After(s.capAt) {
		d = s.capAt
	}
	// The update fails only without a connection (a handler called
	// directly) or when fasthttp has disabled deadline updates; the
	// connection's current deadline then stays in force.
	_ = s.setDeadline(d)
	return nil
}

// WriteTo writes the rest of the body to w, one chunk per write deadline.
// Each chunk is offered to w as an *io.LimitedReader over the source, which
// lets fasthttp's body writer pass an *os.File source to the connection's
// ReadFrom (sendfile). A source that ends early fails with
// io.ErrUnexpectedEOF, since Content-Length promised more.
func (s *fileStream) WriteTo(w io.Writer) (int64, error) {
	var written int64
	for s.remaining > 0 {
		if err := s.refresh(); err != nil {
			return written, err
		}
		s.lr = io.LimitedReader{R: s.src, N: min(s.budget, s.remaining)}
		want := s.lr.N
		n, err := io.Copy(w, &s.lr)
		written += n
		s.remaining -= n
		s.budget -= n
		if err != nil {
			return written, err
		}
		if n < want {
			return written, io.ErrUnexpectedEOF
		}
	}
	return written, nil
}

// SupportsBodyWriteTo reports that WriteTo may replace Read (see
// fasthttp.BodyWriterTo): both produce the same bytes and deadlines.
func (s *fileStream) SupportsBodyWriteTo() bool { return true }

// Read reads the next body bytes, extending the write deadline at the same
// chunk boundaries as WriteTo.
func (s *fileStream) Read(p []byte) (int, error) {
	if s.remaining <= 0 {
		return 0, io.EOF
	}
	if err := s.refresh(); err != nil {
		return 0, err
	}
	if n := min(s.budget, s.remaining); int64(len(p)) > n {
		p = p[:n]
	}
	n, err := s.src.Read(p)
	s.remaining -= int64(n)
	s.budget -= int64(n)
	if err == io.EOF && s.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// Close closes the file. Calls after the first do nothing.
func (s *fileStream) Close() error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
