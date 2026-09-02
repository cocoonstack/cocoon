//go:build linux

package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSparseCopy_AllSparse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	const size = 1 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := SparseCopy(dst, src, Sync); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}

	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if dstInfo.Size() != size {
		t.Errorf("size: got %d, want %d", dstInfo.Size(), size)
	}

	got := readFull(t, dst)
	if !bytes.Equal(got, make([]byte, size)) {
		t.Error("content mismatch: expected all zeros")
	}

	blocks := fileBlocks(t, dst)
	if blocks > 16 {
		t.Errorf("expected sparse dst, got %d blocks", blocks)
	}
}

func TestSparseCopy_PartialData(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	const size = 1 << 20
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}

	data := []byte("hello sparse world!")
	writeAt(t, f, 4096, data)
	f.Close()

	if err := SparseCopy(dst, src, Sync); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}

	srcData := readFull(t, src)
	dstData := readFull(t, dst)
	if !bytes.Equal(srcData, dstData) {
		t.Error("content mismatch")
	}

	blocks := fileBlocks(t, dst)
	fullBlocks := int64(size / 512)
	if blocks >= fullBlocks/2 {
		t.Errorf("expected sparse dst: got %d blocks, full would be %d", blocks, fullBlocks)
	}
}

func TestSparseCopy_NonSparse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	data := bytes.Repeat([]byte("X"), 64*1024)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SparseCopy(dst, src, Sync); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}

	dstData := readFull(t, dst)
	if !bytes.Equal(data, dstData) {
		t.Error("content mismatch")
	}
}

func TestSparseCopy_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SparseCopy(dst, src, Sync); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("expected 0 size, got %d", info.Size())
	}
}

func TestSparseCopy_SrcNotExist(t *testing.T) {
	dir := t.TempDir()
	err := SparseCopy(filepath.Join(dir, "dst"), filepath.Join(dir, "nonexistent"), Sync)
	if err == nil {
		t.Fatal("expected error for nonexistent src")
	}
}

func TestSparseCopy_MultiSegment(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	const size = 256 * 1024
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}

	seg1 := bytes.Repeat([]byte("A"), 4096)
	seg2 := bytes.Repeat([]byte("B"), 4096)
	writeAt(t, f, 64*1024, seg1)
	writeAt(t, f, 128*1024, seg2)
	f.Close()

	if err := SparseCopy(dst, src, Sync); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}

	srcData := readFull(t, src)
	dstData := readFull(t, dst)
	if !bytes.Equal(srcData, dstData) {
		t.Error("content mismatch")
	}

	blocks := fileBlocks(t, dst)
	fullBlocks := int64(size / 512)
	if blocks >= fullBlocks/2 {
		t.Errorf("expected sparse dst: got %d blocks, full would be %d", blocks, fullBlocks)
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

func readFull(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
