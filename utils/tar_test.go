package utils

import (
	"archive/tar"
	"bytes"
	"encoding/json"
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
	dir := t.TempDir()
	src := filepath.Join(dir, "hello.txt")
	content := []byte("hello tar world")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := openAndTarFile(tw, src, "custom-name.txt"); err != nil {
		t.Fatalf("tarFile: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	if hdr.Name != "custom-name.txt" {
		t.Errorf("name: got %q, want %q", hdr.Name, "custom-name.txt")
	}
	if hdr.Size != int64(len(content)) {
		t.Errorf("size: got %d, want %d", hdr.Size, len(content))
	}
	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestTarFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := openAndTarFile(tw, src, "empty.txt"); err != nil {
		t.Fatalf("tarFile: %v", err)
	}
	tw.Close() //nolint:errcheck

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Size != 0 {
		t.Errorf("expected size 0, got %d", hdr.Size)
	}
	data, _ := io.ReadAll(tr)
	if len(data) != 0 {
		t.Errorf("expected empty data, got %d bytes", len(data))
	}
}

func TestTarFile_NotExist(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
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
	// Subdirectory should be skipped.
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := TarDir(tw, dir); err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	tw.Close() //nolint:errcheck

	tr := tar.NewReader(&buf)
	found := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
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
	tw.Close() //nolint:errcheck

	tr := tar.NewReader(&buf)
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF for empty dir, got %v", err)
	}
}

func TestTarDir_NotExist(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
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
	tw.WriteHeader(&tar.Header{Name: "subdir/", Typeflag: tar.TypeDir, Mode: 0o755})           //nolint:errcheck
	tw.WriteHeader(&tar.Header{Name: "file.txt", Size: 3, Typeflag: tar.TypeReg, Mode: 0o644}) //nolint:errcheck
	tw.Write([]byte("abc"))                                                                    //nolint:errcheck
	tw.WriteHeader(&tar.Header{Name: "link", Linkname: "file.txt", Typeflag: tar.TypeSymlink}) //nolint:errcheck
	tw.Close()                                                                                 //nolint:errcheck

	dir := t.TempDir()
	if err := ExtractTar(dir, &buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "file.txt" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected only file.txt, got %v", names)
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
	// Entry whose base name is "." — should be skipped.
	tw.WriteHeader(&tar.Header{Name: ".", Size: 0, Typeflag: tar.TypeReg, Mode: 0o644})      //nolint:errcheck
	tw.WriteHeader(&tar.Header{Name: "ok.txt", Size: 2, Typeflag: tar.TypeReg, Mode: 0o644}) //nolint:errcheck
	tw.Write([]byte("ok"))                                                                   //nolint:errcheck
	tw.Close()                                                                               //nolint:errcheck

	dir := t.TempDir()
	if err := ExtractTar(dir, &buf); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "ok.txt" {
		t.Errorf("unexpected entries: %v", entries)
	}
}

func TestExtractTar_Empty(t *testing.T) {
	buf := makeTar(t, nil)
	dir := t.TempDir()
	if err := ExtractTar(dir, buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected empty dir, got %d entries", len(entries))
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
	tw.Close() //nolint:errcheck

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

func TestExtractTar_Sparse_SingleSegment(t *testing.T) {
	// 1MB logical, one 4KB data segment at offset 8192.
	const realSize = 1024 * 1024
	dataContent := bytes.Repeat([]byte{0xAA}, 4096)
	segments := []sparseSegment{{Offset: 8192, Length: 4096}}

	buf := makeTarSparse(t, "sparse.bin", realSize, segments, dataContent)
	dir := t.TempDir()
	if err := ExtractTar(dir, buf); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "sparse.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != realSize {
		t.Fatalf("logical size: got %d, want %d", len(got), realSize)
	}

	// Before the segment: zeros.
	for i := range 8192 {
		if got[i] != 0 {
			t.Fatalf("byte %d: expected 0, got %d", i, got[i])
		}
	}
	// The segment itself.
	if !bytes.Equal(got[8192:8192+4096], dataContent) {
		t.Error("data segment content mismatch")
	}
	// After the segment: zeros.
	for i := 8192 + 4096; i < realSize; i++ {
		if got[i] != 0 {
			t.Fatalf("byte %d: expected 0, got %d", i, got[i])
		}
	}
}

func TestExtractTar_Sparse_MultipleSegments(t *testing.T) {
	// 64KB logical, three data segments.
	const realSize = 64 * 1024
	seg1 := bytes.Repeat([]byte{0x11}, 4096)
	seg2 := bytes.Repeat([]byte{0x22}, 4096)
	seg3 := bytes.Repeat([]byte{0x33}, 8192)
	segments := []sparseSegment{
		{Offset: 0, Length: 4096},
		{Offset: 16384, Length: 4096},
		{Offset: 49152, Length: 8192},
	}
	data := slices.Concat(seg1, seg2, seg3)

	buf := makeTarSparse(t, "multi.bin", realSize, segments, data)
	dir := t.TempDir()
	if err := ExtractTar(dir, buf); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "multi.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != realSize {
		t.Fatalf("size: got %d, want %d", len(got), realSize)
	}

	// Verify each segment and holes between them.
	if !bytes.Equal(got[0:4096], seg1) {
		t.Error("segment 1 mismatch")
	}
	if !isAllZero(got[4096:16384]) {
		t.Error("hole between seg1 and seg2 not zero")
	}
	if !bytes.Equal(got[16384:16384+4096], seg2) {
		t.Error("segment 2 mismatch")
	}
	if !isAllZero(got[16384+4096 : 49152]) {
		t.Error("hole between seg2 and seg3 not zero")
	}
	if !bytes.Equal(got[49152:49152+8192], seg3) {
		t.Error("segment 3 mismatch")
	}
	if !isAllZero(got[49152+8192:]) {
		t.Error("trailing hole not zero")
	}
}

func TestExtractTar_Sparse_EntireFileIsHole(t *testing.T) {
	// 32KB logical, no data segments at all.
	const realSize = 32 * 1024
	buf := makeTarSparse(t, "allhole.bin", realSize, nil, nil)
	dir := t.TempDir()
	if err := ExtractTar(dir, buf); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "allhole.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != realSize {
		t.Fatalf("size: got %d, want %d", len(got), realSize)
	}
	if !isAllZero(got) {
		t.Error("expected all zeros")
	}
}

func TestExtractTar_Sparse_InvalidMapJSON(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{ //nolint:errcheck
		Name:     "bad.bin",
		Size:     0,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		PAXRecords: map[string]string{
			paxSparseMap:  "not valid json",
			paxSparseSize: "1024",
		},
	})
	tw.Close() //nolint:errcheck

	if err := ExtractTar(t.TempDir(), &buf); err == nil {
		t.Fatal("expected error for invalid sparse map JSON")
	}
}

func TestExtractTar_Sparse_InvalidSizeString(t *testing.T) {
	mapJSON, _ := json.Marshal([]sparseSegment{})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{ //nolint:errcheck
		Name:     "bad.bin",
		Size:     0,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		PAXRecords: map[string]string{
			paxSparseMap:  string(mapJSON),
			paxSparseSize: "not-a-number",
		},
	})
	tw.Close() //nolint:errcheck

	if err := ExtractTar(t.TempDir(), &buf); err == nil {
		t.Fatal("expected error for invalid sparse size")
	}
}

func TestExtractTar_Sparse_MixedWithRegularEntries(t *testing.T) {
	// Archive with one regular file and one sparse file.
	const realSize = 8192
	dataContent := []byte("hello sparse")
	segments := []sparseSegment{{Offset: 4096, Length: int64(len(dataContent))}}
	mapJSON, _ := json.Marshal(segments)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	tw.WriteHeader(&tar.Header{Name: "regular.txt", Size: 5, Typeflag: tar.TypeReg, Mode: 0o644}) //nolint:errcheck
	tw.Write([]byte("hello"))                                                                     //nolint:errcheck

	tw.WriteHeader(&tar.Header{ //nolint:errcheck
		Name:     "sparse.bin",
		Size:     int64(len(dataContent)),
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		PAXRecords: map[string]string{
			paxSparseMap:  string(mapJSON),
			paxSparseSize: strconv.FormatInt(realSize, 10),
		},
	})
	tw.Write(dataContent) //nolint:errcheck
	tw.Close()            //nolint:errcheck

	dir := t.TempDir()
	if err := ExtractTar(dir, &buf); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "regular.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("regular.txt: got %q", got)
	}

	got, err = os.ReadFile(filepath.Join(dir, "sparse.bin"))
	if err != nil {
		t.Fatal(err)
	}
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

func TestExtractFile_AllZeroBlocks(t *testing.T) {
	// 12KB of zeros — extractFile should create holes via seek.
	data := make([]byte, 12*1024)
	dir := t.TempDir()
	path := filepath.Join(dir, "zeros.bin")

	if err := extractFile(path, bytes.NewReader(data), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("size: got %d, want %d", len(got), len(data))
	}
	if !isAllZero(got) {
		t.Error("expected all zeros")
	}
}

func TestExtractFile_NoZeroBlocks(t *testing.T) {
	// Dense data — no blocks should become holes.
	data := bytes.Repeat([]byte{0xBB}, 3*sparseBlockSize)
	dir := t.TempDir()
	path := filepath.Join(dir, "dense.bin")

	if err := extractFile(path, bytes.NewReader(data), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("content mismatch")
	}
}

func TestExtractFile_MixedZeroAndData(t *testing.T) {
	// Pattern: [4KB zeros] [4KB data] [4KB zeros] [4KB data]
	zeroBlock := make([]byte, sparseBlockSize)
	dataBlock := bytes.Repeat([]byte{0xCC}, sparseBlockSize)
	data := slices.Concat(zeroBlock, dataBlock, zeroBlock, dataBlock)

	dir := t.TempDir()
	path := filepath.Join(dir, "mixed.bin")

	if err := extractFile(path, bytes.NewReader(data), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("content mismatch")
	}
}

func TestExtractFile_EndsWithHole(t *testing.T) {
	// 4KB data then 4KB zeros — file must be truncated to 8KB.
	dataBlock := bytes.Repeat([]byte{0xDD}, sparseBlockSize)
	zeroBlock := make([]byte, sparseBlockSize)
	data := slices.Concat(dataBlock, zeroBlock)

	dir := t.TempDir()
	path := filepath.Join(dir, "endhole.bin")

	if err := extractFile(path, bytes.NewReader(data), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch: len got %d, want %d", len(got), len(data))
	}
}

func TestExtractFile_PartialBlock(t *testing.T) {
	// Data that doesn't align to sparseBlockSize.
	data := bytes.Repeat([]byte{0xEE}, sparseBlockSize+100)
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.bin")

	if err := extractFile(path, bytes.NewReader(data), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("content mismatch for partial block")
	}
}

func TestExtractFile_PartialZeroBlock(t *testing.T) {
	// Partial trailing block of zeros — still should produce correct size.
	data := make([]byte, sparseBlockSize+500) // all zeros, not block-aligned
	dir := t.TempDir()
	path := filepath.Join(dir, "pzero.bin")

	if err := extractFile(path, bytes.NewReader(data), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("size: got %d, want %d", len(got), len(data))
	}
	if !isAllZero(got) {
		t.Error("expected all zeros")
	}
}

func TestExtractFile_EmptyInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.bin")

	if err := extractFile(path, bytes.NewReader(nil), 0o644); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("expected empty file, got size %d", fi.Size())
	}
}

func TestExtractFile_SingleByte(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.bin")

	if err := extractFile(path, bytes.NewReader([]byte{0x42}), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{0x42}) {
		t.Errorf("got %v", got)
	}
}

func TestExtractFile_SingleZeroByte(t *testing.T) {
	// A single zero byte — still a valid (tiny) file.
	dir := t.TempDir()
	path := filepath.Join(dir, "onezero.bin")

	if err := extractFile(path, bytes.NewReader([]byte{0x00}), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("expected [0x00], got %v", got)
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

func TestWriteBlockSparse_DataBlock(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck

	chunk := []byte{1, 2, 3, 4}
	hole, err := writeBlockSparse(f, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if hole {
		t.Error("expected hole=false for data block")
	}

	f.Seek(0, io.SeekStart) //nolint:errcheck
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, chunk) {
		t.Errorf("got %v, want %v", got, chunk)
	}
}

func TestWriteBlockSparse_ZeroBlock(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "hole.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck

	chunk := make([]byte, 4096)
	hole, err := writeBlockSparse(f, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if !hole {
		t.Error("expected hole=true for zero block")
	}

	// File position should have advanced by 4096.
	pos, _ := f.Seek(0, io.SeekCurrent)
	if pos != 4096 {
		t.Errorf("position: got %d, want 4096", pos)
	}
}

func TestExtractTar_RoundTrip_LargeFile(t *testing.T) {
	srcDir := t.TempDir()

	// 128KB dense file + small file.
	large := bytes.Repeat([]byte{0xFE}, 128*1024)
	small := []byte("tiny config")
	if err := os.WriteFile(filepath.Join(srcDir, "large.bin"), large, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "small.txt"), small, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := TarDir(tw, srcDir); err != nil {
		t.Fatal(err)
	}
	tw.Close() //nolint:errcheck

	dstDir := t.TempDir()
	if err := ExtractTar(dstDir, &buf); err != nil {
		t.Fatal(err)
	}

	gotLarge, err := os.ReadFile(filepath.Join(dstDir, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotLarge, large) {
		t.Error("large.bin mismatch")
	}

	gotSmall, err := os.ReadFile(filepath.Join(dstDir, "small.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSmall, small) {
		t.Error("small.txt mismatch")
	}
}

// openAndTarFile is a test helper that opens a file and writes it via tarFileFrom.
func openAndTarFile(tw *tar.Writer, path, nameInTar string) error {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return tarFileFrom(tw, f, fi, nameInTar)
}

// makeTar builds a plain tar archive in memory from name→content pairs.
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

// makeTarSparse builds a tar with one COCOON.sparse PAX entry (stored bytes = segments, logical size = realSize).
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
	tw.Close() //nolint:errcheck
	return &buf
}
