//go:build linux

package utils

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

const (
	seekData = 3 // SEEK_DATA
	seekHole = 4 // SEEK_HOLE
)

// SparseCopy copies src to dst preserving sparsity via SEEK_HOLE/SEEK_DATA.
// dst is created as a new file (truncated to src size, then only data segments written).
func SparseCopy(dst, src string) error {
	return copyWithCleanup(dst, src, func(srcFile, dstFile *os.File) error {
		fi, err := srcFile.Stat()
		if err != nil {
			return fmt.Errorf("stat src: %w", err)
		}
		size := fi.Size()
		if size > 0 {
			if truncErr := dstFile.Truncate(size); truncErr != nil {
				return fmt.Errorf("truncate dst: %w", truncErr)
			}
		}

		segments, err := scanDataSegments(int(srcFile.Fd()), size)
		if err != nil {
			return err
		}
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
		return nil
	})
}

// scanDataSegments uses SEEK_DATA/SEEK_HOLE to find all data regions in f.
func scanDataSegments(fd int, size int64) ([]sparseSegment, error) {
	var segments []sparseSegment
	offset := int64(0)

	for offset < size {
		dataStart, err := syscall.Seek(fd, offset, seekData)
		if err != nil {
			// ENXIO means no more data after offset — rest is hole.
			if errors.Is(err, syscall.ENXIO) {
				break
			}
			return nil, fmt.Errorf("seek_data at %d: %w", offset, err)
		}

		holeStart, err := syscall.Seek(fd, dataStart, seekHole)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				// Data extends to EOF.
				holeStart = size
			} else {
				return nil, fmt.Errorf("seek_hole at %d: %w", dataStart, err)
			}
		}

		segments = append(segments, sparseSegment{
			Offset: dataStart,
			Length: holeStart - dataStart,
		})
		offset = holeStart
	}
	return segments, nil
}
