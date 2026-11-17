//go:build linux

package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSparseCopy(t *testing.T) {
	type segment struct {
		off  int64
		data []byte
	}
	tests := []struct {
		name      string
		size      int64
		segs      []segment
		maxBlocks int64
	}{
		{"all sparse", 1 << 20, nil, 16},
		{"partial data", 1 << 20, []segment{{4096, []byte("hello sparse world!")}}, 1<<20/512/2 - 1},
		{"multi segment", 256 * 1024, []segment{{64 * 1024, bytes.Repeat([]byte("A"), 4096)}, {128 * 1024, bytes.Repeat([]byte("B"), 4096)}}, 256*1024/512/2 - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src")
			dst := filepath.Join(dir, "dst")

			f, err := os.Create(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(tt.size); err != nil {
				t.Fatal(err)
			}
			for _, seg := range tt.segs {
				writeAt(t, f, seg.off, seg.data)
			}
			f.Close()

			if err := SparseCopy(dst, src, Sync); err != nil {
				t.Fatalf("SparseCopy: %v", err)
			}

			if !bytes.Equal(readFull(t, src), readFull(t, dst)) {
				t.Error("content mismatch")
			}
			if blocks := fileBlocks(t, dst); blocks > tt.maxBlocks {
				t.Errorf("expected sparse dst: got %d blocks, want at most %d", blocks, tt.maxBlocks)
			}
		})
	}
}

func TestCopySegmentSkipsZeroBlocks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	const size = 8 << 20
	data := make([]byte, size)
	copy(data[size-4:], "tail")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := out.Truncate(size); err != nil {
		t.Fatal(err)
	}

	if err := copySegment(out, in, sparseSegment{Offset: 0, Length: size}, make([]byte, copyBlockSize)); err != nil {
		t.Fatalf("copySegment: %v", err)
	}

	if !bytes.Equal(readFull(t, dst), data) {
		t.Error("content mismatch")
	}
	if blocks, fullBlocks := fileBlocks(t, dst), int64(size/512); blocks >= fullBlocks/2 {
		t.Errorf("expected zero blocks left as holes: got %d blocks, full would be %d", blocks, fullBlocks)
	}
}

func writeAt(t *testing.T, f *os.File, offset int64, data []byte) {
	t.Helper()
	if _, err := f.WriteAt(data, offset); err != nil {
		t.Fatalf("writeAt offset %d: %v", offset, err)
	}
}

func fileBlocks(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("Stat_t not available")
	}
	return st.Blocks
}
