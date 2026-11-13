package compress_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/klauspost/compress/zstd"
)

// poolTestInput returns deterministic, moderately compressible input of n bytes.
func poolTestInput(n int, seed int64) []byte {
	var b strings.Builder
	r := rand.New(rand.NewSource(seed))
	for b.Len() < n {
		fmt.Fprintf(&b, "function f%d(a,b){return a*%d+b;}\n", r.Intn(1000), r.Intn(1000))
	}
	return []byte(b.String()[:n])
}

var poolTestSizes = []int{0, 1, 100, 11 << 10, 300 << 10}

func gunzip(t testing.TB, data []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return got
}

func unzstd(t testing.TB, data []byte) []byte {
	t.Helper()
	d, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer d.Close()
	got, err := d.DecodeAll(data, nil)
	if err != nil {
		t.Fatalf("zstd decode: %v", err)
	}
	return got
}

var zstdLevels = []zstd.EncoderLevel{
	zstd.SpeedFastest, zstd.SpeedDefault, zstd.SpeedBetterCompression, zstd.SpeedBestCompression,
}

func TestGzipBytes_RoundTripsAcrossSizesAndLevels(t *testing.T) {
	for _, level := range []int{-1, 1, 5, 9} {
		for _, size := range poolTestSizes {
			src := poolTestInput(size, int64(size))
			out, err := compress.GzipBytes(src, level)
			if err != nil {
				t.Fatalf("level=%d size=%d: %v", level, size, err)
			}
			if got := gunzip(t, out); !bytes.Equal(got, src) {
				t.Fatalf("level=%d size=%d: round trip mismatch", level, size)
			}
		}
	}
}

func TestZstdBytesLevel_RoundTripsAcrossSizesAndLevels(t *testing.T) {
	for _, level := range zstdLevels {
		for _, size := range poolTestSizes {
			src := poolTestInput(size, int64(size))
			out, err := compress.ZstdBytesLevel(src, level)
			if err != nil {
				t.Fatalf("level=%s size=%d: %v", level, size, err)
			}
			if size == 0 && len(out) == 0 {
				t.Fatalf("level=%s: empty input produced no zstd frame", level)
			}
			if got := unzstd(t, out); !bytes.Equal(got, src) {
				t.Fatalf("level=%s size=%d: round trip mismatch", level, size)
			}
		}
	}
}

// P-F8: cached variants are accounted by capacity, so the encoders must not
// hand back slices with spare capacity.
func TestGzipBytes_ReturnsExactSizeSlice(t *testing.T) {
	for _, size := range poolTestSizes {
		out, err := compress.GzipBytes(poolTestInput(size, 7), 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != cap(out) {
			t.Errorf("size=%d: len=%d cap=%d, want len == cap", size, len(out), cap(out))
		}
	}
}

func TestZstdBytes_ReturnsExactSizeSlice(t *testing.T) {
	for _, size := range poolTestSizes {
		out, err := compress.ZstdBytes(poolTestInput(size, 7))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != cap(out) {
			t.Errorf("size=%d: len=%d cap=%d, want len == cap", size, len(out), cap(out))
		}
	}
}

// Reused encoder state must not change the output: the pooled result has to
// be byte-identical to what a brand-new encoder produces, even after the pool
// was used for other inputs and other levels.
func TestGzipBytes_MatchesFreshEncoderAfterReuse(t *testing.T) {
	for _, level := range []int{1, 5, 9} {
		for _, other := range []int{1, 5, 9} {
			if _, err := compress.GzipBytes(poolTestInput(300<<10, 99), other); err != nil {
				t.Fatal(err)
			}
		}
		src := poolTestInput(11<<10, int64(level))
		var want bytes.Buffer
		zw, err := gzip.NewWriterLevel(&want, level)
		if err != nil {
			t.Fatal(err)
		}
		zw.Write(src) //nolint:errcheck
		zw.Close()

		got, err := compress.GzipBytes(src, level)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Errorf("level=%d: pooled output differs from a fresh gzip.Writer", level)
		}
	}
}

func TestZstdBytesLevel_MatchesFreshEncoderAfterReuse(t *testing.T) {
	for _, level := range zstdLevels {
		for _, other := range zstdLevels {
			if _, err := compress.ZstdBytesLevel(poolTestInput(300<<10, 99), other); err != nil {
				t.Fatal(err)
			}
		}
		src := poolTestInput(11<<10, int64(level))
		fresh, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
		want := fresh.EncodeAll(src, nil)
		fresh.Close()

		got, err := compress.ZstdBytesLevel(src, level)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("level=%s: pooled output differs from a fresh encoder", level)
		}
	}
}

// A returned slice must not share memory with pooled scratch space that a
// later call overwrites.
func TestGzipAndZstdBytes_OutputSurvivesLaterCalls(t *testing.T) {
	x := poolTestInput(11<<10, 1)
	y := poolTestInput(11<<10, 2)

	gx, err := compress.GzipBytes(x, 5)
	if err != nil {
		t.Fatal(err)
	}
	zx, err := compress.ZstdBytes(x)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := compress.GzipBytes(y, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := compress.ZstdBytes(y); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(gunzip(t, gx), x) {
		t.Error("gzip output was clobbered by a later call")
	}
	if !bytes.Equal(unzstd(t, zx), x) {
		t.Error("zstd output was clobbered by a later call")
	}
}

// Run with -race: concurrent callers share the pools. Every output is kept
// and verified only after all goroutines finished, so late corruption of a
// returned slice by another goroutine's reuse is detected.
func TestGzipAndZstdBytes_ConcurrentRoundTrip(t *testing.T) {
	const workers, iters = 12, 25
	type result struct {
		src, gz, zst []byte
	}
	results := make([][]result, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				size := poolTestSizes[(w+i)%len(poolTestSizes)]
				if size > 64<<10 {
					size = 64 << 10
				}
				src := poolTestInput(size, int64(w*1000+i))
				gz, err := compress.GzipBytes(src, 1+(w+i)%9)
				if err != nil {
					t.Error(err)
					return
				}
				zst, err := compress.ZstdBytesLevel(src, zstdLevels[(w+i)%2])
				if err != nil {
					t.Error(err)
					return
				}
				results[w] = append(results[w], result{src, gz, zst})
			}
		}(w)
	}
	wg.Wait()
	for w := range results {
		for i, r := range results[w] {
			if !bytes.Equal(gunzip(t, r.gz), r.src) {
				t.Fatalf("worker %d iter %d: gzip round trip mismatch", w, i)
			}
			if !bytes.Equal(unzstd(t, r.zst), r.src) {
				t.Fatalf("worker %d iter %d: zstd round trip mismatch", w, i)
			}
		}
	}
}

func TestZstdBytesLevel_InvalidLevelReturnsError(t *testing.T) {
	for _, level := range []zstd.EncoderLevel{0, 5, 99, -1} {
		for i := 0; i < 2; i++ { // the second call must not hit a broken cached pool
			if _, err := compress.ZstdBytesLevel([]byte("payload"), level); err == nil {
				t.Errorf("level=%d call %d: expected an error", level, i)
			}
		}
	}
	if _, err := compress.ZstdBytes([]byte("payload")); err != nil {
		t.Fatalf("valid level failed after invalid ones: %v", err)
	}
}

// Encoders that the pool drops are never Closed; they must not own
// background goroutines.
func TestZstdBytes_PooledEncodersDoNotLeakGoroutines(t *testing.T) {
	src := poolTestInput(300<<10, 3)
	for _, level := range zstdLevels { // warm every pool
		if _, err := compress.ZstdBytesLevel(src, level); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		for _, level := range zstdLevels {
			if _, err := compress.ZstdBytesLevel(src[:(i+1)*1000], level); err != nil {
				t.Fatal(err)
			}
		}
		if i%10 == 0 {
			runtime.GC()
		}
	}
	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines grew from %d to %d", before, after)
	}
}
