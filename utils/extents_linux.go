//go:build linux

package utils

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// fsIocFiemap is FS_IOC_FIEMAP; x/sys/unix does not define it.
	fsIocFiemap uintptr = 0xC020660B

	fiemapFlagSync        uint32 = 0x1
	fiemapExtentLast      uint32 = 0x1
	fiemapExtentUnwritten uint32 = 0x800

	fiemapBatch = 512
)

type fiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	reserved64 [2]uint64
	Flags      uint32
	reserved   [3]uint32
}

type fiemapRequest struct {
	Start   uint64
	Length  uint64
	Flags   uint32
	Mapped  uint32
	Count   uint32
	_       uint32
	Extents [fiemapBatch]fiemapExtent
}

// FileExtents flushes f and returns its extents in logical order.
func FileExtents(f *os.File) ([]Extent, error) {
	var out []Extent
	req := &fiemapRequest{Length: ^uint64(0), Flags: fiemapFlagSync, Count: fiemapBatch}
	for {
		req.Mapped = 0
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(req))); errno != 0 { //nolint:gosec
			return nil, fmt.Errorf("fiemap %s: %w", f.Name(), errno)
		}
		if req.Mapped == 0 {
			return out, nil
		}
		for _, e := range req.Extents[:req.Mapped] {
			out = append(out, Extent{
				Logical:   int64(e.Logical),  //nolint:gosec
				Physical:  int64(e.Physical), //nolint:gosec
				Length:    int64(e.Length),   //nolint:gosec
				Unwritten: e.Flags&fiemapExtentUnwritten != 0,
			})
		}
		last := req.Extents[req.Mapped-1]
		if last.Flags&fiemapExtentLast != 0 {
			return out, nil
		}
		req.Start = last.Logical + last.Length
		req.Length = ^uint64(0) - req.Start
	}
}

// PunchHole deallocates a range of f and keeps its size.
func PunchHole(f *os.File, offset, length int64) error {
	if err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, offset, length); err != nil {
		return fmt.Errorf("punch hole in %s at %d len %d: %w", f.Name(), offset, length, err)
	}
	return nil
}
