package oci

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestScanBootFilesNamePrefixLengths(t *testing.T) {
	tests := []struct {
		name       string
		namePrefix string
	}{
		{"empty prefix", ""},
		{"import placeholder under 12 chars", "import-0"},
		{"exactly 12 chars", "twelve_chars"},
		{"real sha256 hex", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tr := writeBootTar(t, "boot/vmlinuz-6.8.0", "boot/initrd.img-6.8.0")
			kernel, initrd, err := scanBootFiles(t.Context(), tr, dir, tt.namePrefix)
			if err != nil {
				t.Fatalf("scanBootFiles: %v", err)
			}
			if want := filepath.Join(dir, tt.namePrefix+".vmlinuz"); kernel != want {
				t.Errorf("kernel = %q, want %q", kernel, want)
			}
			if want := filepath.Join(dir, tt.namePrefix+".initrd.img"); initrd != want {
				t.Errorf("initrd = %q, want %q", initrd, want)
			}
			for _, p := range []string{kernel, initrd} {
				if _, statErr := os.Stat(p); statErr != nil {
					t.Errorf("expected extracted file %s: %v", p, statErr)
				}
			}
		})
	}
}

func TestScanBootFilesSkipsNonBoot(t *testing.T) {
	dir := t.TempDir()
	tr := writeBootTar(t, "usr/bin/vmlinuz-decoy", "boot/vmlinuz-6.8.0.old", "etc/initrd.img")
	kernel, initrd, err := scanBootFiles(t.Context(), tr, dir, "import-0")
	if err != nil {
		t.Fatalf("scanBootFiles: %v", err)
	}
	if kernel != "" || initrd != "" {
		t.Errorf("expected no boot files extracted, got kernel=%q initrd=%q", kernel, initrd)
	}
}

func writeBootTar(t *testing.T, paths ...string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, p := range paths {
		body := []byte("payload:" + p)
		if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("write header %s: %v", p, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write body %s: %v", p, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}
