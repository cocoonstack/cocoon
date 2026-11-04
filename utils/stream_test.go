package utils

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	defer stream.Close() //nolint:errcheck
	if err := ExtractTar(t.TempDir(), stream); err == nil {
		t.Fatal("ExtractTar accepted an archive that is missing b-memory-range")
	}
}

func TestPeekReader_FullRead(t *testing.T) {
	src := strings.NewReader("hello world")
	head, full, err := PeekReader(src, 5)
	if err != nil {
		t.Fatalf("PeekReader: %v", err)
	}
	if string(head) != "hello" {
		t.Errorf("head = %q, want %q", head, "hello")
	}
	rest, err := io.ReadAll(full)
	if err != nil {
		t.Fatalf("read full: %v", err)
	}
	if string(rest) != "hello world" {
		t.Errorf("full = %q, want %q", rest, "hello world")
	}
}

func TestPeekReader_ShortStream(t *testing.T) {
	src := strings.NewReader("hi")
	head, full, err := PeekReader(src, 8)
	if err != nil {
		t.Fatalf("short stream should not error, got %v", err)
	}
	if string(head) != "hi" {
		t.Errorf("head = %q, want %q (truncated)", head, "hi")
	}
	rest, err := io.ReadAll(full)
	if err != nil {
		t.Fatalf("read full: %v", err)
	}
	if string(rest) != "hi" {
		t.Errorf("full = %q, want %q", rest, "hi")
	}
}

func TestPeekReader_Empty(t *testing.T) {
	head, full, err := PeekReader(strings.NewReader(""), 4)
	if err != nil {
		t.Fatalf("empty stream should not error, got %v", err)
	}
	if len(head) != 0 {
		t.Errorf("head len = %d, want 0", len(head))
	}
	rest, _ := io.ReadAll(full)
	if len(rest) != 0 {
		t.Errorf("full len = %d, want 0", len(rest))
	}
}

func TestPeekReader_Binary(t *testing.T) {
	data := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00}
	head, full, err := PeekReader(bytes.NewReader(data), 2)
	if err != nil {
		t.Fatalf("PeekReader: %v", err)
	}
	if !bytes.Equal(head, []byte{0x1f, 0x8b}) {
		t.Errorf("head = %v, want gzip magic", head)
	}
	rest, _ := io.ReadAll(full)
	if !bytes.Equal(rest, data) {
		t.Errorf("full = %v, want %v", rest, data)
	}
}
