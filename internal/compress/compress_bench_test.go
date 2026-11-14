package compress_test

import (
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/compress"
)

// benchAsset is the ~11 KB JavaScript asset used by the performance review
// harness, so numbers here are comparable with the recorded baseline.
func benchAsset() []byte {
	return []byte(strings.Repeat("console.log('hello world, this is a benchmark asset');\n", 200))
}

func BenchmarkGzipBytes(b *testing.B) {
	src := benchAsset()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := compress.GzipBytes(src, 5); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkZstdBytes(b *testing.B) {
	src := benchAsset()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := compress.ZstdBytes(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGzipBytesParallel(b *testing.B) {
	src := benchAsset()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := compress.GzipBytes(src, 5); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkZstdBytesParallel(b *testing.B) {
	src := benchAsset()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := compress.ZstdBytes(src); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
