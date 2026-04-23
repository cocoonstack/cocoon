//go:build linux

package utils

import (
	"fmt"
	"os"
	"syscall"
)

// ficlone is the ioctl number for btrfs/xfs/bcachefs CoW file cloning.
const ficlone = 0x40049409

// ReflinkCopy copies a single file, preferring FICLONE (O(1) CoW on
// btrfs/xfs/bcachefs) and falling back to SparseCopy on any error.
func ReflinkCopy(dst, src string) error {
	if err := tryFiclone(dst, src); err == nil {
		return nil
	}
	return SparseCopy(dst, src)
}

func tryFiclone(dst, src string) error {
	srcFile, err := os.Open(src) //nolint:gosec
	if err != nil {
		return err
	}
	defer srcFile.Close() //nolint:errcheck

	dstFile, err := os.Create(dst) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() {
		dstFile.Close() //nolint:errcheck,gosec
		if err != nil {
			os.Remove(dst) //nolint:errcheck,gosec
		}
	}()

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dstFile.Fd(), ficlone, srcFile.Fd())
	if errno != 0 {
		err = fmt.Errorf("ficlone: %w", errno)
		return err
	}
	return nil
}
