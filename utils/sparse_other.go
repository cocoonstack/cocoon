//go:build !linux

package utils

import (
	"io"
	"os"
)

// SparseCopy copies src to dst. On non-Linux platforms, sparsity is not preserved.
func SparseCopy(dst, src string) error {
	return copyWithCleanup(dst, src, func(srcFile, dstFile *os.File) error {
		_, err := io.Copy(dstFile, srcFile)
		return err
	})
}
