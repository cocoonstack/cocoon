package hypervisor

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cocoonstack/cocoon/utils"
)

const (
	diskDeltaInfix = ".delta."

	// deltaLengthSize is the byte count of the big-endian header length that opens a delta file.
	deltaLengthSize = 8
)

// diskDelta heads a delta file; the bytes of each non-zero range follow in order.
type diskDelta struct {
	Size   int64        `json:"size"`
	Ranges []deltaRange `json:"ranges"`
}

type deltaRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
	Zero   bool  `json:"zero,omitzero"`
}

// mapping is where one logical position lives; the zero value is a hole.
type mapping struct {
	physical  int64
	unwritten bool
	mapped    bool
}

// ApplyDiskDeltas replays rounds 1..rounds of each named disk from srcDir onto its copy in dstDir.
func ApplyDiskDeltas(ctx context.Context, dstDir, srcDir string, disks []string, rounds int) error {
	_, err := utils.Map(ctx, disks, func(_ context.Context, _ int, name string) (struct{}, error) {
		for n := 1; n <= rounds; n++ {
			if err := applyDiskDelta(filepath.Join(dstDir, name), filepath.Join(srcDir, diskDeltaName(name, n))); err != nil {
				return struct{}{}, fmt.Errorf("apply %s round %d: %w", name, n, err)
			}
		}
		return struct{}{}, nil
	})
	return err
}

func diskDeltaName(disk string, round int) string {
	return disk + diskDeltaInfix + strconv.Itoa(round)
}

// writeDiskDelta records how src differs from ref; the two must share extents through reflink, so an extent both map holds the same bytes.
func writeDiskDelta(dst, ref, src string) error {
	srcFile, err := os.Open(src) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open delta source: %w", err)
	}
	defer srcFile.Close() //nolint:errcheck
	fi, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("stat delta source: %w", err)
	}
	srcExtents, err := utils.FileExtents(srcFile)
	if err != nil {
		return err
	}
	refFile, err := os.Open(ref) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open delta reference: %w", err)
	}
	defer refFile.Close() //nolint:errcheck
	refExtents, err := utils.FileExtents(refFile)
	if err != nil {
		return err
	}
	header := diskDelta{Size: fi.Size(), Ranges: diffExtents(refExtents, srcExtents, fi.Size())}
	return writeDeltaFile(dst, &header, srcFile)
}

// diffExtents returns the ranges below size where cur and ref map different storage; a range cur reads as zeros is marked Zero.
func diffExtents(ref, cur []utils.Extent, size int64) []deltaRange {
	var out []deltaRange
	for pos := int64(0); pos < size; {
		refAt, refEnd := extentAt(&ref, pos)
		curAt, curEnd := extentAt(&cur, pos)
		end := min(refEnd, curEnd, size)
		if refAt != curAt {
			zero := curAt == (mapping{}) || curAt.unwritten
			if n := len(out) - 1; n >= 0 && out[n].Zero == zero && out[n].Offset+out[n].Length == pos {
				out[n].Length += end - pos
			} else {
				out = append(out, deltaRange{Offset: pos, Length: end - pos, Zero: zero})
			}
		}
		pos = end
	}
	return out
}

// extentAt advances *extents past pos and returns the mapping at pos plus the offset where it stops holding.
func extentAt(extents *[]utils.Extent, pos int64) (mapping, int64) {
	for len(*extents) > 0 && (*extents)[0].Logical+(*extents)[0].Length <= pos {
		*extents = (*extents)[1:]
	}
	if len(*extents) == 0 {
		return mapping{}, math.MaxInt64
	}
	e := (*extents)[0]
	if pos < e.Logical {
		return mapping{}, e.Logical
	}
	return mapping{physical: e.Physical + pos - e.Logical, unwritten: e.Unwritten, mapped: true}, e.Logical + e.Length
}

func writeDeltaFile(dst string, header *diskDelta, src *os.File) (err error) {
	raw, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("encode delta header: %w", err)
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create delta: %w", err)
	}
	defer func() {
		err = errors.Join(err, out.Close())
		if err != nil {
			_ = os.Remove(tmp)
			return
		}
		err = os.Rename(tmp, dst)
	}()
	if err = binary.Write(out, binary.BigEndian, uint64(len(raw))); err != nil {
		return fmt.Errorf("write delta header: %w", err)
	}
	if _, err = out.Write(raw); err != nil {
		return fmt.Errorf("write delta header: %w", err)
	}
	for _, r := range header.Ranges {
		if r.Zero {
			continue
		}
		if _, err = src.Seek(r.Offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek delta source to %d: %w", r.Offset, err)
		}
		if _, err = io.CopyN(out, src, r.Length); err != nil {
			return fmt.Errorf("copy range at %d len %d: %w", r.Offset, r.Length, err)
		}
	}
	return nil
}

func applyDiskDelta(dst, delta string) error {
	in, err := os.Open(delta) //nolint:gosec
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck
	header, err := readDeltaHeader(in)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck
	if err = out.Truncate(header.Size); err != nil {
		return fmt.Errorf("resize %s: %w", dst, err)
	}
	for _, r := range header.Ranges {
		if r.Zero {
			if err = utils.PunchHole(out, r.Offset, r.Length); err != nil {
				return err
			}
			continue
		}
		if _, err = out.Seek(r.Offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek %s to %d: %w", dst, r.Offset, err)
		}
		if _, err = io.CopyN(out, in, r.Length); err != nil {
			return fmt.Errorf("write range at %d len %d: %w", r.Offset, r.Length, err)
		}
	}
	return nil
}

// readDeltaHeader leaves f at the first data byte and rejects a file whose size disagrees with its header.
func readDeltaHeader(f *os.File) (*diskDelta, error) {
	var length uint64
	if err := binary.Read(f, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read delta header length: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if length > uint64(fi.Size()) { //nolint:gosec
		return nil, fmt.Errorf("delta header length %d exceeds file size %d", length, fi.Size())
	}
	raw := make([]byte, length)
	if _, err = io.ReadFull(f, raw); err != nil {
		return nil, fmt.Errorf("read delta header: %w", err)
	}
	var header diskDelta
	if err = json.Unmarshal(raw, &header); err != nil {
		return nil, fmt.Errorf("decode delta header: %w", err)
	}
	want := deltaLengthSize + int64(length)
	for _, r := range header.Ranges {
		if !r.Zero {
			want += r.Length
		}
	}
	if fi.Size() != want {
		return nil, fmt.Errorf("delta is %d bytes, its header needs %d: the transfer is incomplete", fi.Size(), want)
	}
	return &header, nil
}
