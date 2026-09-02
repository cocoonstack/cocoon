package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReflinkCopy_BasicContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	want := []byte("hello reflink test content")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ReflinkCopy(t.Context(), dst, src, Sync); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content mismatch: got %q, want %q", got, want)
	}
}

func TestReflinkCopy_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ReflinkCopy(t.Context(), dst, src, Sync); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("size: got %d, want 0", info.Size())
	}
}

func TestReflinkCopy_LargeFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	want := bytes.Repeat([]byte("REFLINK"), 32768) // 224KB
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ReflinkCopy(t.Context(), dst, src, Sync); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("content mismatch for large file")
	}
}

func TestReflinkCopy_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(dst, []byte("old data"), 0o644); err != nil {
		t.Fatal(err)
	}

	want := []byte("new data")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ReflinkCopy(t.Context(), dst, src, NoSync); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content: got %q, want %q", got, want)
	}
}

func TestReflinkCopy_SrcNotExist(t *testing.T) {
	dir := t.TempDir()
	err := ReflinkCopy(t.Context(), filepath.Join(dir, "dst"), filepath.Join(dir, "nonexistent"), Sync)
	if err == nil {
		t.Fatal("expected error for nonexistent src")
	}
}

func TestReflinkCopy_DstDirNotExist(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := ReflinkCopy(filepath.Join(dir, "nodir", "dst"), src, Sync)
	if err == nil {
		t.Fatal("expected error for nonexistent dst directory")
	}
}

func TestReflinkCopy_PreservesSize(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	want := bytes.Repeat([]byte{0x42}, 65536)
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ReflinkCopy(t.Context(), dst, src, Sync); err != nil {
		t.Fatal(err)
	}

	srcInfo, _ := os.Stat(src)
	dstInfo, _ := os.Stat(dst)
	if srcInfo.Size() != dstInfo.Size() {
		t.Errorf("size mismatch: src=%d dst=%d", srcInfo.Size(), dstInfo.Size())
	}
}
