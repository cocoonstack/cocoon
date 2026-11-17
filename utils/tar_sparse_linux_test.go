//go:build linux

package utils

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTarFileMaybeSparse_FallsBackOnLargeMap(t *testing.T) {
	limitSparseMap(t, 4*1024)

	path := filepath.Join(t.TempDir(), "memory-ranges")
	logicalSize := writeSparseFile(t, path, 200, 4096)

	tr := tar.NewReader(tarMaybeSparse(t, path, "memory-ranges"))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "memory-ranges" {
		t.Fatalf("name = %q, want memory-ranges", hdr.Name)
	}
	if _, ok := hdr.PAXRecords[paxSparseMap]; ok {
		t.Errorf("PAX %s present, expected non-sparse fallback", paxSparseMap)
	}
	if hdr.Size != logicalSize {
		t.Errorf("hdr.Size = %d, want logical size %d", hdr.Size, logicalSize)
	}
}

func TestTarFileMaybeSparse_FallbackRoundTrip(t *testing.T) {
	limitSparseMap(t, 4*1024)

	path := filepath.Join(t.TempDir(), "memory-ranges")
	writeSparseFile(t, path, 200, 4096)

	dst := t.TempDir()
	if err := ExtractTar(dst, tarMaybeSparse(t, path, "memory-ranges")); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	got := readFull(t, filepath.Join(dst, "memory-ranges"))
	want := readFull(t, path)
	if !bytes.Equal(got, want) {
		t.Errorf("extracted content differs (len got=%d want=%d)", len(got), len(want))
	}
}

func TestTarFileMaybeSparse_PreservesSparsePathForSmallMaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small-sparse")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, f, 0, bytes.Repeat([]byte{0xAA}, 4096))
	writeAt(t, f, 1<<20, bytes.Repeat([]byte{0xBB}, 4096))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(tarMaybeSparse(t, path, "small-sparse"))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hdr.PAXRecords[paxSparseMap]; !ok {
		t.Error("expected sparse PAX record for small fragmentation, got none")
	}
}

func tarMaybeSparse(t *testing.T, path, name string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarFileMaybeSparse(tw, path, name); err != nil {
		t.Fatalf("tarFileMaybeSparse: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func limitSparseMap(t *testing.T, n int) {
	t.Helper()
	orig := maxSparseMapJSONSize
	maxSparseMapJSONSize = n
	t.Cleanup(func() { maxSparseMapJSONSize = orig })
}

func writeSparseFile(t *testing.T, path string, numSegments, blockSize int) int64 {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := range numSegments {
		offset := int64(i) * int64(blockSize) * 2
		data := bytes.Repeat([]byte{byte((i % 250) + 1)}, blockSize)
		if _, err := f.WriteAt(data, offset); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
