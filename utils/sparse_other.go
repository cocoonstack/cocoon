//go:build !linux

package utils

import (
	"errors"
	"io"
	"os"
)

// SparseCopy copies src to dst without preserving sparsity.
func SparseCopy(dst, src string, mode SyncMode) error {
	return CopyWithCleanup(dst, src, func(srcFile, dstFile *os.File) error {
		if _, err := io.Copy(dstFile, srcFile); err != nil {
			return err
		}
		if mode == NoSync {
			return nil
		}
		return dstFile.Sync()
	})
}

func scanDataSegments(int, int64) ([]sparseSegment, error) {
	return nil, errors.ErrUnsupported
}
