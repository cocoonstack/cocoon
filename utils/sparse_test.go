package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSparseCopy_BasicContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	want := []byte("hello world, this is a test file for sparse copy")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SparseCopy(dst, src, Sync); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
}

func TestSparseCopy_EmptyFile_CrossPlatform(t *testing.T) {
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
		t.Errorf("size: got %d, want 0", info.Size())
	}
}

func TestSparseCopy_LargeFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	// 256KB of repeated data.
	want := bytes.Repeat([]byte("ABCDEFGHIJKLMNOP"), 16384)
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SparseCopy(dst, src, Sync); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("content mismatch for large file")
	}
}

func TestSparseCopy_SrcNotExist_CrossPlatform(t *testing.T) {
	dir := t.TempDir()
	err := SparseCopy(filepath.Join(dir, "dst"), filepath.Join(dir, "nonexistent"), Sync)
	if err == nil {
		t.Fatal("expected error for nonexistent src")
	}
}

func TestSparseCopy_DstDirNotExist(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := SparseCopy(filepath.Join(dir, "nodir", "dst"), src, Sync)
	if err == nil {
		t.Fatal("expected error for nonexistent dst directory")
	}
}

func TestSparseCopy_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(dst, []byte("old content that should be overwritten"), 0o644); err != nil {
		t.Fatal(err)
	}

	want := []byte("new content")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SparseCopy(dst, src, NoSync); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content: got %q, want %q", got, want)
	}
}

func TestSparseCopy_PreservesSize(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	want := bytes.Repeat([]byte{0x42}, 65536)
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SparseCopy(dst, src, Sync); err != nil {
		t.Fatal(err)
	}

	srcInfo, _ := os.Stat(src)
	dstInfo, _ := os.Stat(dst)
	if srcInfo.Size() != dstInfo.Size() {
		t.Errorf("size mismatch: src=%d dst=%d", srcInfo.Size(), dstInfo.Size())
	}
}
