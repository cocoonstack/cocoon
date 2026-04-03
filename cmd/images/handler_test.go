package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

func gzipWrap(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarWrap(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(data)),
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestDetectReader(t *testing.T) {
	qcow2Data := append(qcow2Magic, make([]byte, 100)...)
	tarData := []byte("this is a tar-like stream of data, not really tar but not qcow2 either")

	tests := []struct {
		name     string
		input    []byte
		wantType imageType
		wantData []byte
	}{
		{
			name:     "raw qcow2",
			input:    qcow2Data,
			wantType: imageTypeQcow2,
			wantData: qcow2Data,
		},
		{
			name:     "gzip qcow2",
			input:    gzipWrap(t, qcow2Data),
			wantType: imageTypeQcow2,
			wantData: qcow2Data,
		},
		{
			name:     "raw tar",
			input:    tarData,
			wantType: imageTypeTar,
			wantData: tarData,
		},
		{
			name:     "gzip tar",
			input:    gzipWrap(t, tarData),
			wantType: imageTypeTar,
			wantData: tarData,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, typ, cleanup, err := detectReader(bytes.NewReader(tt.input))
			if err != nil {
				t.Fatalf("detectReader: %v", err)
			}
			defer cleanup()

			if typ != tt.wantType {
				t.Errorf("type: got %d, want %d", typ, tt.wantType)
			}

			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, tt.wantData) {
				t.Errorf("data mismatch: got %d bytes, want %d", len(got), len(tt.wantData))
			}
		})
	}
}

func TestDetectReader_TooShort(t *testing.T) {
	_, _, _, err := detectReader(bytes.NewReader([]byte{0x00}))
	if err == nil {
		t.Fatal("expected error for 1-byte input")
	}
}

func TestDetectReader_Empty(t *testing.T) {
	_, _, _, err := detectReader(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestDetectReader_GzipPreservesFullContent(t *testing.T) {
	original := make([]byte, 16384)
	for i := range original {
		original[i] = byte(i % 251)
	}

	gz := gzipWrap(t, original)
	r, typ, cleanup, err := detectReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("detectReader: %v", err)
	}
	defer cleanup()

	if typ != imageTypeTar {
		t.Errorf("type: got %d, want imageTypeTar", typ)
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("content mismatch: got %d bytes, want %d", len(got), len(original))
	}
}

func TestDetectLocalImportSource(t *testing.T) {
	dir := t.TempDir()
	qcow2Path := writeTempFile(t, dir, "image.qcow2", append(qcow2Magic, make([]byte, 32)...))
	tarPath := writeTempFile(t, dir, "image.tar", tarWrap(t, "payload.txt", []byte("payload")))
	gzipTarPath := writeTempFile(t, dir, "image.tar.gz", gzipWrap(t, tarWrap(t, "payload.txt", []byte("payload"))))
	invalidPath := writeTempFile(t, dir, "invalid.bin", []byte("not an archive"))

	tests := []struct {
		name     string
		path     string
		wantKind importSourceKind
		wantErr  bool
	}{
		{name: "qcow2", path: qcow2Path, wantKind: importSourceQcow2},
		{name: "tar", path: tarPath, wantKind: importSourceTar},
		{name: "gzip tar", path: gzipTarPath, wantKind: importSourceStream},
		{name: "invalid", path: invalidPath, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := detectLocalImportSource(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("detectLocalImportSource(%q) unexpectedly succeeded", tt.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("detectLocalImportSource(%q): %v", tt.path, err)
			}
			if got != tt.wantKind {
				t.Fatalf("detectLocalImportSource(%q) = %v, want %v", tt.path, got, tt.wantKind)
			}
		})
	}
}

func TestPlanLocalImportPreservesAllFiles(t *testing.T) {
	dir := t.TempDir()
	part1 := writeTempFile(t, dir, "part-1.qcow2", append(qcow2Magic, make([]byte, 8)...))
	part2 := writeTempFile(t, dir, "part-2.qcow2", []byte("second-part"))

	plan, err := planLocalImport([]string{part1, part2})
	if err != nil {
		t.Fatalf("planLocalImport: %v", err)
	}
	if plan.kind != importSourceQcow2 {
		t.Fatalf("plan.kind = %v, want %v", plan.kind, importSourceQcow2)
	}
	if len(plan.files) != 2 {
		t.Fatalf("len(plan.files) = %d, want 2", len(plan.files))
	}
	if plan.files[0] != part1 || plan.files[1] != part2 {
		t.Fatalf("plan.files = %#v, want [%q %q]", plan.files, part1, part2)
	}
}
