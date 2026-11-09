//go:build linux

package utils

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const copyBlockSize = 1 << 20

// SparseCopy copies src to dst preserving sparsity via SEEK_HOLE/SEEK_DATA; dst is truncated to src size and only data segments are written.
func SparseCopy(dst, src string, mode SyncMode) error {
	return CopyWithCleanup(dst, src, func(srcFile, dstFile *os.File) error {
		fi, err := srcFile.Stat()
		if err != nil {
			return fmt.Errorf("stat src: %w", err)
		}
		size := fi.Size()
		if truncErr := dstFile.Truncate(size); truncErr != nil {
			return fmt.Errorf("truncate dst: %w", truncErr)
		}

		segments, err := scanDataSegments(int(srcFile.Fd()), size)
		if err != nil {
			return err
		}
		var zeroSkipBuf []byte
		if onFUSE(srcFile) {
			zeroSkipBuf = make([]byte, copyBlockSize)
		}
		for _, seg := range segments {
			if err := copySegment(dstFile, srcFile, seg, zeroSkipBuf); err != nil {
				return fmt.Errorf("copy segment at %d len %d: %w", seg.Offset, seg.Length, err)
			}
		}

		if mode == NoSync {
			return nil
		}
		if err := dstFile.Sync(); err != nil {
			return fmt.Errorf("sync dst: %w", err)
		}
		return nil
	})
}

func scanDataSegments(fd int, size int64) ([]sparseSegment, error) {
	var segments []sparseSegment
	offset := int64(0)

	for offset < size {
		dataStart, err := syscall.Seek(fd, offset, unix.SEEK_DATA)
		if err != nil {
			// ENXIO means no more data after offset — rest is hole.
			if errors.Is(err, syscall.ENXIO) {
				break
			}
			return nil, fmt.Errorf("seek_data at %d: %w", offset, err)
		}

		holeStart, err := syscall.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("seek_hole at %d: %w", dataStart, err)
		}

		segments = append(segments, sparseSegment{
			Offset: dataStart,
			Length: holeStart - dataStart,
		})
		offset = holeStart
	}
	return segments, nil
}

func copySegment(dst, src *os.File, seg sparseSegment, zeroSkipBuf []byte) error {
	if zeroSkipBuf == nil {
		if _, err := src.Seek(seg.Offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := dst.Seek(seg.Offset, io.SeekStart); err != nil {
			return err
		}
		_, err := io.CopyN(dst, src, seg.Length)
		return err
	}
	for pos, end := seg.Offset, seg.Offset+seg.Length; pos < end; {
		block := zeroSkipBuf[:min(int64(len(zeroSkipBuf)), end-pos)]
		if _, err := src.ReadAt(block, pos); err != nil {
			return err
		}
		if !isZero(block) {
			if _, err := dst.WriteAt(block, pos); err != nil {
				return err
			}
		}
		pos += int64(len(block))
	}
	return nil
}

func onFUSE(f *os.File) bool {
	var st unix.Statfs_t
	return unix.Fstatfs(int(f.Fd()), &st) == nil && st.Type == unix.FUSE_SUPER_MAGIC
}

func isZero(data []byte) bool {
	return data[0] == 0 && bytes.Equal(data[1:], data[:len(data)-1])
}
