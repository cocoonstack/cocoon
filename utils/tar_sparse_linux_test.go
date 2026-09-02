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
	orig := maxSparseMapJSONSize
	maxSparseMapJSONSize = 4 * 1024
	t.Cleanup(func() { maxSparseMapJSONSize = orig })

	dir := t.TempDir()
	path := filepath.Join(dir, "memory-ranges")
	logicalSize := writeSparseFile(t, path, 200, 4096)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarFileMaybeSparse(tw, path, "memory-ranges"); err != nil {
		t.Fatalf("tarFileMaybeSparse: %v (expected fallback to succeed)", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
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
	orig := maxSparseMapJSONSize
	maxSparseMapJSONSize = 4 * 1024
	t.Cleanup(func() { maxSparseMapJSONSize = orig })

	dir := t.TempDir()
	path := filepath.Join(dir, "memory-ranges")
	writeSparseFile(t, path, 200, 4096)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarFileMaybeSparse(tw, path, "memory-ranges"); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := ExtractTar(dst, &buf); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "memory-ranges"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xAA}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xBB}, 4096), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarFileMaybeSparse(tw, path, "small-sparse"); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hdr.PAXRecords[paxSparseMap]; !ok {
		t.Errorf("expected sparse PAX record for small fragmentation, got none")
	}
}

func writeSparseFile(t *testing.T, path string, numSegments, blockSize int) int64 {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
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
