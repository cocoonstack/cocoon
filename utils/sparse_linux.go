//go:build linux

package utils

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

const (
	seekData = 3 // SEEK_DATA
	seekHole = 4 // SEEK_HOLE
)

type dataSegment struct {
	Offset int64
	Length int64
}

// scanDataSegments uses SEEK_DATA/SEEK_HOLE to find all data regions in f.
func scanDataSegments(fd int, size int64) ([]dataSegment, error) {
	var segments []dataSegment
	offset := int64(0)

	for offset < size {
		// Find next data start.
		dataStart, err := syscall.Seek(fd, offset, seekData)
		if err != nil {
			// ENXIO means no more data after offset — rest is hole.
			if err == syscall.ENXIO {
				break
			}
			return nil, fmt.Errorf("SEEK_DATA at %d: %w", offset, err)
		}

		// Find the end of this data segment (start of next hole).
		holeStart, err := syscall.Seek(fd, dataStart, seekHole)
		if err != nil {
			if err == syscall.ENXIO {
				// Data extends to EOF.
				holeStart = size
			} else {
				return nil, fmt.Errorf("SEEK_HOLE at %d: %w", dataStart, err)
			}
		}

		segments = append(segments, dataSegment{
			Offset: dataStart,
			Length: holeStart - dataStart,
		})
		offset = holeStart
	}
	return segments, nil
}

// SparseCopy copies src to dst preserving sparsity via SEEK_HOLE/SEEK_DATA.
// dst is created as a new file (truncated to src size, then only data segments written).
func SparseCopy(dst, src string) error {
	srcFile, err := os.Open(src) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer srcFile.Close() //nolint:errcheck

	fi, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}
	size := fi.Size()

	dstFile, err := os.Create(dst) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			dstFile.Close() //nolint:errcheck,gosec
			os.Remove(dst)  //nolint:errcheck,gosec
		}
	}()

	// Pre-allocate the full logical size (all holes).
	if size > 0 {
		if truncErr := dstFile.Truncate(size); truncErr != nil {
			return fmt.Errorf("truncate dst: %w", truncErr)
		}
	}

	segments, err := scanDataSegments(int(srcFile.Fd()), size)
	if err != nil {
		return err
	}

	// Copy only data segments.
	for _, seg := range segments {
		if _, err := srcFile.Seek(seg.Offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek src to %d: %w", seg.Offset, err)
		}
		if _, err := dstFile.Seek(seg.Offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek dst to %d: %w", seg.Offset, err)
		}
		if _, err := io.CopyN(dstFile, srcFile, seg.Length); err != nil {
			return fmt.Errorf("copy segment at %d len %d: %w", seg.Offset, seg.Length, err)
		}
	}

	if err := dstFile.Sync(); err != nil {
		return fmt.Errorf("sync dst: %w", err)
	}
	cleanup = false
	return dstFile.Close()
}
