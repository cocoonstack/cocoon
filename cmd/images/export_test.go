package images

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingExportReader struct {
	read bool
}

func (r *failingExportReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("read failed")
}

func TestWriteExportFileReplacesOnlyAfterCompleteCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.qcow2")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeExportFile(path, &failingExportReader{}); err == nil {
		t.Fatal("expected copy failure")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("existing output changed after failed export: %q", got)
	}
	if err := writeExportFile(path, strings.NewReader("replacement")); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "replacement" {
		t.Fatalf("output = %q, want replacement", got)
	}
}
