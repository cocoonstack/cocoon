//go:build linux

package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileExtentsReportsDataAndSkipsHoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err = f.Truncate(1 << 22); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err = f.WriteAt(make([]byte, 4096), 1<<21); err != nil {
		t.Fatalf("write: %v", err)
	}
	extents, err := FileExtents(f)
	if err != nil {
		t.Skipf("filesystem has no FIEMAP: %v", err)
	}
	if len(extents) == 0 {
		t.Fatal("no extents after a flushed write")
	}
	first, last := extents[0], extents[len(extents)-1]
	if first.Logical > 1<<21 || last.Logical+last.Length < 1<<21+4096 {
		t.Errorf("extents %+v do not cover the write", extents)
	}
	if first.Logical < 1<<21 {
		t.Errorf("extents %+v cover the leading hole", extents)
	}
}

func TestPunchHoleKeepsSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if _, err = f.WriteAt([]byte{1, 2, 3, 4}, 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err = PunchHole(f, 0, 2); err != nil {
		t.Skipf("filesystem has no hole punch: %v", err)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != 4 {
		t.Errorf("size = %d, want 4", fi.Size())
	}
}
