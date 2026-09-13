//go:build !linux

package utils

import (
	"errors"
	"io"
	"os"
)

// SparseCopy copies src to dst. On non-Linux platforms, sparsity is not preserved.
func SparseCopy(dst, src string, sync SyncMode) error {
	return copyWithCleanup(dst, src, func(srcFile, dstFile *os.File) error {
		if _, err := io.Copy(dstFile, srcFile); err != nil {
			return err
		}
		if !sync {
			return nil
		}
		return dstFile.Sync()
	})
}

func scanDataSegments(int, int64) ([]sparseSegment, error) {
	return nil, errors.ErrUnsupported
}
