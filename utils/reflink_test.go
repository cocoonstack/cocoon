package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReflinkCopy(t *testing.T) {
	testFileCopy(t, func(dst, src string, mode SyncMode) error {
		return ReflinkCopy(t.Context(), dst, src, mode)
	})
}

func testFileCopy(t *testing.T, copyFn func(dst, src string, mode SyncMode) error) {
	t.Helper()
	tests := []struct {
		name     string
		content  []byte
		existing []byte
		mode     SyncMode
	}{
		{name: "basic content", content: []byte("hello copy test content"), mode: Sync},
		{name: "empty file", mode: Sync},
		{name: "large file", content: bytes.Repeat([]byte("ABCDEFGHIJKLMNOP"), 16384), mode: Sync},
		{name: "overwrites existing", content: []byte("new content"), existing: []byte("old content that should be overwritten"), mode: NoSync},
		{name: "preserves size", content: bytes.Repeat([]byte{0x42}, 65536), mode: Sync},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src")
			dst := filepath.Join(dir, "dst")
			if err := os.WriteFile(src, tt.content, 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.existing != nil {
				if err := os.WriteFile(dst, tt.existing, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := copyFn(dst, src, tt.mode); err != nil {
				t.Fatalf("copy: %v", err)
			}

			if got := readFull(t, dst); !bytes.Equal(got, tt.content) {
				t.Errorf("content mismatch: got %d bytes, want %d bytes", len(got), len(tt.content))
			}
		})
	}

	t.Run("src not exist", func(t *testing.T) {
		dir := t.TempDir()
		if err := copyFn(filepath.Join(dir, "dst"), filepath.Join(dir, "nonexistent"), Sync); err == nil {
			t.Fatal("expected error for nonexistent src")
		}
	})

	t.Run("dst dir not exist", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := copyFn(filepath.Join(dir, "nodir", "dst"), src, Sync); err == nil {
			t.Fatal("expected error for nonexistent dst directory")
		}
	})
}
