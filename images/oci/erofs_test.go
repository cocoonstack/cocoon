package oci

import (
	"archive/tar"
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	erofsTypeReg    = "1"
	erofsTypeDir    = "2"
	erofsTypeChrdev = "3"
)

func TestErofsVersionAtLeast(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{"current", "mkfs.erofs (erofs-utils) 1.8.10\navailable compressors: lz4, lz4hc", false},
		{"floor exact", "mkfs.erofs (erofs-utils) 1.8", false},
		{"double digit minor", "mkfs.erofs (erofs-utils) 1.10.0", false},
		{"next major", "mkfs.erofs (erofs-utils) 2.0", false},
		{"ubuntu 24.04 stock", "mkfs.erofs (erofs-utils) 1.7.1", true},
		{"ancient", "mkfs.erofs (erofs-utils) 1.5", true},
		{"unparseable", "command not found", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := erofsVersionAtLeast(tt.output)
			if (err != nil) != tt.wantErr {
				t.Errorf("erofsVersionAtLeast(%q) = %v, wantErr %v", tt.output, err, tt.wantErr)
			}
		})
	}
}

func TestBlobHex(t *testing.T) {
	const want = "b1910ad92b38c2711e188378a0c3e41bc91a32e857795511a1d06b2a1581375c"
	if got := blobHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); got != want {
		t.Fatalf("blobHex() = %s, want %s", got, want)
	}
}

func TestRunErofsConversionWhiteouts(t *testing.T) {
	if _, err := exec.LookPath("dump.erofs"); err != nil {
		t.Skip("dump.erofs not installed")
	}
	if err := checkErofsVersion(t.Context()); err != nil {
		t.Skipf("mkfs.erofs unusable: %v", err)
	}

	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, hdr := range []*tar.Header{
		{Name: "a/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "a/.wh.x", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: ".wh.b", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "c/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "c/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "c/keep", Typeflag: tar.TypeReg, Mode: 0o644},
	} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %s: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	dir := t.TempDir()
	image := filepath.Join(dir, "layer.erofs")
	if _, _, err := runErofsConversion(t.Context(), &layer, dir, "layer", "00000000-0000-5000-8000-000000000000", image); err != nil {
		t.Fatalf("runErofsConversion: %v", err)
	}

	tests := []struct {
		path string
		want map[string]string
	}{
		{"/", map[string]string{"a": erofsTypeDir, "b": erofsTypeChrdev, "c": erofsTypeDir}},
		{"/a", map[string]string{"x": erofsTypeChrdev}},
		{"/c", map[string]string{"keep": erofsTypeReg}},
	}
	for _, tt := range tests {
		got := listErofsDir(t, image, tt.path)
		if len(got) != len(tt.want) {
			t.Errorf("%s entries = %v, want %v", tt.path, got, tt.want)
			continue
		}
		for name, typ := range tt.want {
			if got[name] != typ {
				t.Errorf("%s type = %q, want %q (entries %v)", filepath.Join(tt.path, name), got[name], typ, got)
			}
		}
	}
}

func listErofsDir(t *testing.T, image, path string) map[string]string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "dump.erofs", "--ls", "--path="+path, image).CombinedOutput()
	if err != nil {
		t.Fatalf("dump.erofs --ls %s: %v (output: %s)", path, err, out)
	}
	_, listing, ok := strings.Cut(string(out), "FILENAME")
	if !ok {
		t.Fatalf("dump.erofs --ls %s: no listing in %q", path, out)
	}
	entries := map[string]string{}
	for line := range strings.Lines(listing) {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] == "." || fields[2] == ".." {
			continue
		}
		entries[fields[2]] = fields[1]
	}
	return entries
}
