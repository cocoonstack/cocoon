package utils

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestTarDirStreamFailsTheReaderOnAnUnreadableEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a-config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b-memory-range"), []byte("mem"), 0o000); err != nil {
		t.Fatal(err)
	}
	stream := TarDirStream(src, nil)
	defer stream.Close()
	if err := ExtractTar(t.TempDir(), stream); err == nil {
		t.Fatal("ExtractTar accepted an archive that is missing b-memory-range")
	}
}

func TestPeekReader(t *testing.T) {
	tests := []struct {
		name     string
		in       []byte
		n        int
		wantHead []byte
	}{
		{"full read", []byte("hello world"), 5, []byte("hello")},
		{"short stream is truncated", []byte("hi"), 8, []byte("hi")},
		{"empty", nil, 4, nil},
		{"binary", []byte{0x1f, 0x8b, 0x08, 0x00, 0x00}, 2, []byte{0x1f, 0x8b}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head, full, err := PeekReader(bytes.NewReader(tt.in), tt.n)
			if err != nil {
				t.Fatalf("PeekReader: %v", err)
			}
			if !bytes.Equal(head, tt.wantHead) {
				t.Errorf("head = %q, want %q", head, tt.wantHead)
			}
			rest, err := io.ReadAll(full)
			if err != nil {
				t.Fatalf("read full: %v", err)
			}
			if !bytes.Equal(rest, tt.in) {
				t.Errorf("full = %q, want %q", rest, tt.in)
			}
		})
	}
}
