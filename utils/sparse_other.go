//go:build !linux

package utils

import (
	"fmt"
	"io"
	"os"
)

// SparseCopy copies src to dst. On non-Linux platforms, sparsity is not preserved.
func SparseCopy(dst, src string) error {
	srcFile, err := os.Open(src) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer srcFile.Close() //nolint:errcheck

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

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	cleanup = false
	return dstFile.Close()
}
