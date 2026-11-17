package utils

import (
	"archive/tar"
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestTarFile(t *testing.T) {
	tests := []struct {
		name      string
		srcName   string
		nameInTar string
		content   []byte
	}{
		{"renamed", "hello.txt", "custom-name.txt", []byte("hello tar world")},
		{"empty file", "empty.txt", "empty.txt", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), tt.srcName)
			if err := os.WriteFile(src, tt.content, 0o644); err != nil {
				t.Fatal(err)
			}

			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err := openAndTarFile(tw, src, tt.nameInTar); err != nil {
				t.Fatalf("openAndTarFile: %v", err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}

			tr := tar.NewReader(&buf)
			hdr, err := tr.Next()
			if err != nil {
				t.Fatalf("tar.Next: %v", err)
			}
			if hdr.Name != tt.nameInTar {
				t.Errorf("name: got %q, want %q", hdr.Name, tt.nameInTar)
			}
			if hdr.Size != int64(len(tt.content)) {
				t.Errorf("size: got %d, want %d", hdr.Size, len(tt.content))
			}
			got, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.content) {
				t.Errorf("content mismatch: got %q, want %q", got, tt.content)
			}
			if _, err := tr.Next(); !errors.Is(err, io.EOF) {
				t.Errorf("expected EOF, got %v", err)
			}
		})
	}
}

func TestTarFile_NotExist(t *testing.T) {
	tw := tar.NewWriter(io.Discard)
	if err := openAndTarFile(tw, "/nonexistent/file.txt", "file.txt"); err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestTarDir(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"a.txt":    []byte("aaa"),
		"b.bin":    []byte("bbb"),
		"c.config": []byte("ccc"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := TarDir(tw, dir); err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	tw.Close()

	tr := tar.NewReader(&buf)
	found := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		found[hdr.Name] = data
	}

	if len(found) != len(files) {
		t.Errorf("entry count: got %d, want %d", len(found), len(files))
	}
	for name, want := range files {
		got, ok := found[name]
		if !ok {
			t.Errorf("missing entry %q", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestTarDir_Empty(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := TarDir(tw, dir); err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	tw.Close()

	tr := tar.NewReader(&buf)
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF for empty dir, got %v", err)
	}
}

func TestTarDir_NotExist(t *testing.T) {
	tw := tar.NewWriter(io.Discard)
	if err := TarDir(tw, "/nonexistent/dir"); err == nil {
		t.Fatal("expected error for nonexistent dir")
	}
}

func TestExtractTar(t *testing.T) {
	files := map[string][]byte{
		"a.txt": []byte("hello"),
		"b.bin": []byte("world"),
	}
	buf := makeTar(t, files)

	dir := t.TempDir()
	if err := ExtractTar(dir, buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestExtractTar_SkipsDirectories(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "subdir/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "file.txt", Size: 3, Typeflag: tar.TypeReg, Mode: 0o644})
	tw.Write([]byte("abc"))
	tw.WriteHeader(&tar.Header{Name: "link", Linkname: "file.txt", Typeflag: tar.TypeSymlink})
	tw.Close()

	dir := t.TempDir()
	if err := ExtractTar(dir, &buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	if got := dirNames(t, dir); !slices.Equal(got, []string{"file.txt"}) {
		t.Errorf("expected only file.txt, got %v", got)
	}
}

func TestExtractTar_PathTraversal(t *testing.T) {
	files := map[string][]byte{
		"../../../etc/passwd": []byte("evil"),
		"normal.txt":          []byte("safe"),
	}
	buf := makeTar(t, files)

	dir := t.TempDir()
	if err := ExtractTar(dir, buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "passwd"))
	if err != nil {
		t.Fatalf("expected passwd to be extracted (sanitized): %v", err)
	}
	if !bytes.Equal(got, []byte("evil")) {
		t.Errorf("passwd content: got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "etc")); err == nil {
		t.Error("path traversal was not prevented")
	}
}

func TestExtractTar_SkipsDotNames(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	tw.WriteHeader(&tar.Header{Name: ".", Size: 0, Typeflag: tar.TypeReg, Mode: 0o644})
	tw.WriteHeader(&tar.Header{Name: "ok.txt", Size: 2, Typeflag: tar.TypeReg, Mode: 0o644})
	tw.Write([]byte("ok"))
	tw.Close()

	dir := t.TempDir()
	if err := ExtractTar(dir, &buf); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"ok.txt"}) {
		t.Errorf("unexpected entries: %v", got)
	}
}

func TestExtractTar_Empty(t *testing.T) {
	buf := makeTar(t, nil)
	dir := t.TempDir()
	if err := ExtractTar(dir, buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}
	if got := dirNames(t, dir); len(got) != 0 {
		t.Errorf("expected empty dir, got %d entries", len(got))
	}
}

func TestExtractTar_InvalidData(t *testing.T) {
	if err := ExtractTar(t.TempDir(), strings.NewReader("not a tar")); err == nil {
		t.Fatal("expected error for invalid tar")
	}
}

func TestExtractTar_RoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	files := map[string][]byte{
		"config.json":   []byte(`{"key":"value"}`),
		"state.json":    []byte(`{"state":"paused"}`),
		"memory-ranges": bytes.Repeat([]byte{0xAB}, 1024),
		"large.bin":     bytes.Repeat([]byte{0xFE}, 128*1024),
		"small.txt":     []byte("tiny config"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(srcDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := TarDir(tw, srcDir); err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	tw.Close()

	dstDir := t.TempDir()
	if err := ExtractTar(dstDir, &buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dstDir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s content mismatch", name)
		}
	}
}

func TestExtractTar_Sparse(t *testing.T) {
	tests := []struct {
		name     string
		realSize int64
		segments []sparseSegment
		data     []byte
	}{
		{
			name:     "single segment",
			realSize: 1024 * 1024,
			segments: []sparseSegment{{Offset: 8192, Length: 4096}},
			data:     bytes.Repeat([]byte{0xAA}, 4096),
		},
		{
			name:     "multiple segments",
			realSize: 64 * 1024,
			segments: []sparseSegment{{Offset: 0, Length: 4096}, {Offset: 16384, Length: 4096}, {Offset: 49152, Length: 8192}},
			data:     slices.Concat(bytes.Repeat([]byte{0x11}, 4096), bytes.Repeat([]byte{0x22}, 4096), bytes.Repeat([]byte{0x33}, 8192)),
		},
		{
			name:     "entire file is hole",
			realSize: 32 * 1024,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := ExtractTar(dir, makeTarSparse(t, "sparse.bin", tt.realSize, tt.segments, tt.data)); err != nil {
				t.Fatal(err)
			}

			got := readFull(t, filepath.Join(dir, "sparse.bin"))
			if want := expandSparse(tt.realSize, tt.segments, tt.data); !bytes.Equal(got, want) {
				t.Errorf("content differs from the expanded sparse map (len got=%d want=%d)", len(got), len(want))
			}
		})
	}
}

func TestExtractTar_Sparse_InvalidPAXRecords(t *testing.T) {
	tests := []struct {
		name string
		pax  map[string]string
	}{
		{"map json", map[string]string{paxSparseMap: "not valid json", paxSparseSize: "1024"}},
		{"size string", map[string]string{paxSparseMap: "[]", paxSparseSize: "not-a-number"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			tw.WriteHeader(&tar.Header{
				Name:       "bad.bin",
				Size:       0,
				Mode:       0o644,
				Typeflag:   tar.TypeReg,
				PAXRecords: tt.pax,
			})
			tw.Close()

			if err := ExtractTar(t.TempDir(), &buf); err == nil {
				t.Fatalf("expected error for invalid sparse %s", tt.name)
			}
		})
	}
}

func TestExtractFileSparse_RejectsInvalidMap(t *testing.T) {
	const realSize = 4096
	tests := []struct {
		name     string
		segments []sparseSegment
		dataSize int64
		wantErr  string
	}{
		{
			name:     "offset past real size",
			segments: []sparseSegment{{Offset: realSize + 1, Length: 0}},
			wantErr:  "sparse map segment 4097+0 exceeds the 4096-byte file",
		},
		{
			name:     "negative length",
			segments: []sparseSegment{{Offset: 0, Length: -1}},
			wantErr:  "sparse map segment 0+-1 exceeds the 4096-byte file",
		},
		{
			name:     "data sum differs from entry size",
			segments: []sparseSegment{{Offset: 0, Length: 100}, {Offset: 1024, Length: 28}},
			dataSize: 64,
			wantErr:  "sparse map describes 128 data bytes, entry holds 64",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapJSON, err := json.Marshal(tt.segments)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "disk.raw")
			err = extractFileSparse(path, bytes.NewReader(make([]byte, tt.dataSize)), 0o644, realSize, tt.dataSize, string(mapJSON))
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestExtractTar_Sparse_MixedWithRegularEntries(t *testing.T) {
	const realSize = 8192
	dataContent := []byte("hello sparse")
	segments := []sparseSegment{{Offset: 4096, Length: int64(len(dataContent))}}
	mapJSON, _ := json.Marshal(segments)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	tw.WriteHeader(&tar.Header{Name: "regular.txt", Size: 5, Typeflag: tar.TypeReg, Mode: 0o644})
	tw.Write([]byte("hello"))

	tw.WriteHeader(&tar.Header{
		Name:     "sparse.bin",
		Size:     int64(len(dataContent)),
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		PAXRecords: map[string]string{
			paxSparseMap:  string(mapJSON),
			paxSparseSize: strconv.FormatInt(realSize, 10),
		},
	})
	tw.Write(dataContent)
	tw.Close()

	dir := t.TempDir()
	if err := ExtractTar(dir, &buf); err != nil {
		t.Fatal(err)
	}

	got := readFull(t, filepath.Join(dir, "regular.txt"))
	if string(got) != "hello" {
		t.Errorf("regular.txt: got %q", got)
	}

	got = readFull(t, filepath.Join(dir, "sparse.bin"))
	if int64(len(got)) != realSize {
		t.Fatalf("sparse.bin size: got %d, want %d", len(got), realSize)
	}
	if !isAllZero(got[:4096]) {
		t.Error("leading hole not zero")
	}
	if !bytes.Equal(got[4096:4096+len(dataContent)], dataContent) {
		t.Error("sparse data mismatch")
	}
}

func TestExtractFile_RunsAcrossReadBuffer(t *testing.T) {
	data := make([]byte, extractReadBuf+3*sparseBlockSize)
	for i := range sparseBlockSize {
		data[i] = 0xA5
	}
	for i := extractReadBuf + sparseBlockSize; i < extractReadBuf+2*sparseBlockSize; i++ {
		data[i] = 0x5A
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "memory", Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := ExtractTar(dir, &buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}
	got := readFull(t, filepath.Join(dir, "memory"))
	if !bytes.Equal(got, data) {
		t.Fatalf("extracted %d bytes differ from the %d-byte source (zero run across the read buffer, trailing hole)", len(got), len(data))
	}
}

func TestExtractFile(t *testing.T) {
	dataBlock := bytes.Repeat([]byte{0xCC}, sparseBlockSize)
	tests := []struct {
		name string
		data []byte
	}{
		{"all zero blocks", make([]byte, 12*1024)},
		{"no zero blocks", bytes.Repeat([]byte{0xBB}, 3*sparseBlockSize)},
		{"mixed zero and data", slices.Concat(zeroBlock[:], dataBlock, zeroBlock[:], dataBlock)},
		{"ends with hole", slices.Concat(bytes.Repeat([]byte{0xDD}, sparseBlockSize), zeroBlock[:])},
		{"partial block", bytes.Repeat([]byte{0xEE}, sparseBlockSize+100)},
		{"partial zero block", make([]byte, sparseBlockSize+500)},
		{"empty input", nil},
		{"single byte", []byte{0x42}},
		{"single zero byte", []byte{0x00}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.bin")
			if err := extractFile(path, bytes.NewReader(tt.data), 0o644, int64(len(tt.data))); err != nil {
				t.Fatal(err)
			}
			if got := readFull(t, path); !bytes.Equal(got, tt.data) {
				t.Errorf("content mismatch: got %d bytes, want %d bytes", len(got), len(tt.data))
			}
		})
	}
}

func TestIsAllZero(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want bool
	}{
		{"nil", nil, true},
		{"empty", []byte{}, true},
		{"single zero", []byte{0}, true},
		{"single nonzero", []byte{1}, false},
		{"all zeros", make([]byte, 4096), true},
		{"last byte nonzero", append(make([]byte, 4095), 0xFF), false},
		{"first byte nonzero", append([]byte{0x01}, make([]byte, 4095)...), false},
		{"middle byte nonzero", func() []byte {
			b := make([]byte, 4096)
			b[2048] = 0x01
			return b
		}(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAllZero(tt.in); got != tt.want {
				t.Errorf("isAllZero: got %v, want %v", got, tt.want)
			}
		})
	}
}

func openAndTarFile(tw *tar.Writer, path, nameInTar string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return rewindAndTarFull(tw, f, fi, path, nameInTar)
}

func makeTar(t *testing.T, files map[string][]byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Size:     int64(len(data)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func makeTarSparse(t *testing.T, name string, realSize int64, segments []sparseSegment, data []byte) *bytes.Buffer {
	t.Helper()
	mapJSON, err := json.Marshal(segments)
	if err != nil {
		t.Fatal(err)
	}

	var dataSize int64
	for _, s := range segments {
		dataSize += s.Length
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Size:     dataSize,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		PAXRecords: map[string]string{
			paxSparseMap:  string(mapJSON),
			paxSparseSize: strconv.FormatInt(realSize, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	return &buf
}

func expandSparse(realSize int64, segments []sparseSegment, data []byte) []byte {
	want := make([]byte, realSize)
	for _, seg := range segments {
		copy(want[seg.Offset:], data[:seg.Length])
		data = data[seg.Length:]
	}
	return want
}
