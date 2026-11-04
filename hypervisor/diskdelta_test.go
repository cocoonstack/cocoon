package hypervisor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cocoonstack/cocoon/utils"
)

func TestDiffExtents(t *testing.T) {
	tests := []struct {
		name string
		ref  []utils.Extent
		cur  []utils.Extent
		size int64
		want []deltaRange
	}{
		{
			name: "shared extents differ nowhere",
			ref:  []utils.Extent{{Logical: 0, Physical: 100, Length: 10}},
			cur:  []utils.Extent{{Logical: 0, Physical: 100, Length: 10}},
			size: 10,
		},
		{
			name: "split extent with the same storage differs nowhere",
			ref:  []utils.Extent{{Logical: 0, Physical: 100, Length: 10}},
			cur:  []utils.Extent{{Logical: 0, Physical: 100, Length: 4}, {Logical: 4, Physical: 104, Length: 6}},
			size: 10,
		},
		{
			name: "rewritten middle",
			ref:  []utils.Extent{{Logical: 0, Physical: 100, Length: 10}},
			cur:  []utils.Extent{{Logical: 0, Physical: 100, Length: 4}, {Logical: 4, Physical: 900, Length: 2}, {Logical: 6, Physical: 106, Length: 4}},
			size: 10,
			want: []deltaRange{{Offset: 4, Length: 2}},
		},
		{
			name: "new data in a hole",
			cur:  []utils.Extent{{Logical: 2, Physical: 900, Length: 2}},
			size: 10,
			want: []deltaRange{{Offset: 2, Length: 2}},
		},
		{
			name: "discarded data",
			ref:  []utils.Extent{{Logical: 2, Physical: 100, Length: 2}},
			size: 10,
			want: []deltaRange{{Offset: 2, Length: 2, Zero: true}},
		},
		{
			name: "unwritten extent reads as zeros",
			ref:  []utils.Extent{{Logical: 0, Physical: 100, Length: 4}},
			cur:  []utils.Extent{{Logical: 0, Physical: 900, Length: 4, Unwritten: true}},
			size: 4,
			want: []deltaRange{{Offset: 0, Length: 4, Zero: true}},
		},
		{
			name: "written over a shared unwritten extent",
			ref:  []utils.Extent{{Logical: 0, Physical: 100, Length: 4, Unwritten: true}},
			cur:  []utils.Extent{{Logical: 0, Physical: 100, Length: 4}},
			size: 4,
			want: []deltaRange{{Offset: 0, Length: 4}},
		},
		{
			name: "adjacent changes of one kind merge",
			ref:  []utils.Extent{{Logical: 0, Physical: 100, Length: 4}},
			cur:  []utils.Extent{{Logical: 0, Physical: 900, Length: 2}, {Logical: 2, Physical: 500, Length: 2}, {Logical: 4, Physical: 700, Length: 2}},
			size: 6,
			want: []deltaRange{{Offset: 0, Length: 6}},
		},
		{
			name: "extent past the end of the file is cut",
			cur:  []utils.Extent{{Logical: 0, Physical: 900, Length: 100}},
			size: 10,
			want: []deltaRange{{Offset: 0, Length: 10}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diffExtents(tt.ref, tt.cur, tt.size); !slices.Equal(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDiskDeltaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref")
	src := filepath.Join(dir, "src")
	writeSparse(t, ref, 1<<20, map[int64]string{0: "base", 1 << 19: "kept"})
	writeSparse(t, src, 2<<20, map[int64]string{0: "next", 1 << 19: "kept", 3 << 19: "grown"})
	delta := filepath.Join(dir, diskDeltaName("disk", 1))
	if err := writeDiskDelta(delta, ref, src); err != nil {
		skipWithoutFiemap(t, err)
		t.Fatalf("writeDiskDelta: %v", err)
	}
	if err := applyDiskDelta(ref, delta); err != nil {
		t.Fatalf("applyDiskDelta: %v", err)
	}
	got, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("applied disk differs from the source")
	}
}

func TestApplyDiskDeltaPunchesZeroRanges(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	writeSparse(t, dst, 1<<20, map[int64]string{0: "stale"})
	src, err := os.Create(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer src.Close()
	delta := filepath.Join(dir, "delta")
	if err = writeDeltaFile(delta, &diskDelta{Size: 1 << 20, Ranges: []deltaRange{{Offset: 0, Length: 4096, Zero: true}}}, src); err != nil {
		t.Fatalf("writeDeltaFile: %v", err)
	}
	if err = applyDiskDelta(dst, delta); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skip("needs fallocate")
		}
		t.Fatalf("applyDiskDelta: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got[:5], make([]byte, 5)) {
		t.Errorf("range reads %q, want zeros", got[:5])
	}
}

func TestApplyDiskDeltaRejectsATruncatedFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	writeSparse(t, src, 4096, map[int64]string{0: "data"})
	srcFile, err := os.Open(src)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer srcFile.Close()
	delta := filepath.Join(dir, "delta")
	if err = writeDeltaFile(delta, &diskDelta{Size: 4096, Ranges: []deltaRange{{Offset: 0, Length: 4096}}}, srcFile); err != nil {
		t.Fatalf("writeDeltaFile: %v", err)
	}
	fi, err := os.Stat(delta)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err = os.Truncate(delta, fi.Size()-1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	dst := filepath.Join(dir, "dst")
	writeSparse(t, dst, 4096, nil)
	if err = applyDiskDelta(dst, delta); !errors.Is(err, io.EOF) {
		t.Errorf("applyDiskDelta: %v, want EOF", err)
	}
}

func TestApplyDiskDeltasFailsOnAMissingRound(t *testing.T) {
	dst, src := t.TempDir(), t.TempDir()
	writeSparse(t, filepath.Join(dst, "disk"), 4096, nil)
	err := ApplyDiskDeltas(t.Context(), dst, src, []string{"disk"}, 1)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ApplyDiskDeltas: %v, want not exist", err)
	}
}

func writeSparse(t *testing.T, path string, size int64, chunks map[int64]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err = f.Truncate(size); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for off, data := range chunks {
		if _, err = f.WriteAt([]byte(data), off); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}
