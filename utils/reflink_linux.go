//go:build linux

package utils

import (
	"fmt"
	"os"
	"syscall"
)

// ficlone is the ioctl number for btrfs/xfs/bcachefs CoW file cloning.
const ficlone = 0x40049409

// ReflinkCopy copies a single file, preferring FICLONE (O(1) CoW on btrfs/xfs/bcachefs) and falling back to SparseCopy on any error.
func ReflinkCopy(dst, src string, sync SyncMode) error {
	if err := tryFiclone(dst, src); err == nil {
		return nil
	}
	return SparseCopy(dst, src, sync)
}

func tryFiclone(dst, src string) error {
	return copyWithCleanup(dst, src, func(srcFile, dstFile *os.File) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dstFile.Fd(), ficlone, srcFile.Fd()); errno != 0 {
			return fmt.Errorf("ficlone: %w", errno)
		}
		return nil
	})
}
