package drop

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Both cases receive 8 MiB through the production extraction and publication
// path, including file sync and macOS quarantine. Transport is excluded.
func BenchmarkExtract(b *testing.B) {
	for _, files := range []int{1, 256} {
		b.Run(fmt.Sprintf("%d-files", files), func(b *testing.B) {
			const total = 8 << 20
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			for i := 0; i < files; i++ {
				size := int64(total / files)
				if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("file-%d", i), Mode: 0600, Size: size}); err != nil {
					b.Fatal(err)
				}
				if _, err := io.CopyN(tw, zeroStream{}, size); err != nil {
					b.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				b.Fatal(err)
			}
			archive.WriteString(Completion)
			parent := b.TempDir()
			b.SetBytes(total)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dir := filepath.Join(parent, "drop")
				if _, err := Extract(bytes.NewReader(archive.Bytes()), Options{Dir: dir}); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := os.RemoveAll(dir); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

// BenchmarkQuarantine isolates the per-file native helper cost on macOS.
func BenchmarkQuarantine(b *testing.B) {
	if runtime.GOOS != "darwin" {
		b.Skip("macOS quarantine")
	}
	path := filepath.Join(b.TempDir(), "file")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		quarantine(context.Background(), path)
	}
}
