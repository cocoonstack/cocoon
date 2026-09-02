//go:build linux

package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/projecteru2/core/log"
)

// ficlone is the ioctl number for btrfs/xfs/bcachefs CoW file cloning.
const ficlone = 0x40049409

// noReflink remembers the filesystems whose FICLONE answered "not supported", so later copies on them skip the create/ioctl/unlink round trip.
var noReflink sync.Map

// ReflinkCopy copies a single file, preferring FICLONE (O(1) CoW on btrfs/xfs/bcachefs) and falling back to SparseCopy on any error.
func ReflinkCopy(ctx context.Context, dst, src string, sync SyncMode) error {
	fs, known := fsID(filepath.Dir(dst))
	if _, unsupported := noReflink.Load(fs); known && unsupported {
		return SparseCopy(dst, src, sync)
	}
	err := tryFiclone(dst, src, sync)
	if err == nil {
		return nil
	}
	if known && reflinkUnsupported(err) {
		if _, seen := noReflink.LoadOrStore(fs, struct{}{}); !seen {
			log.WithFunc("utils.ReflinkCopy").Warnf(ctx, "reflink unsupported on the filesystem holding %s (%v); clones copy their disks in full", filepath.Dir(dst), err)
		}
	}
	return SparseCopy(dst, src, sync)
}

func tryFiclone(dst, src string, sync SyncMode) error {
	return copyWithCleanup(dst, src, func(srcFile, dstFile *os.File) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dstFile.Fd(), ficlone, srcFile.Fd()); errno != 0 {
			return fmt.Errorf("ficlone: %w", errno)
		}
		// FICLONE only shares extents — still honor Sync, or the fast path silently drops durability.
		if sync == Sync {
			if err := dstFile.Sync(); err != nil {
				return fmt.Errorf("sync dst: %w", err)
			}
		}
		return nil
	})
}

func fsID(dir string) (syscall.Fsid, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return syscall.Fsid{}, false
	}
	return st.Fsid, true
}

// reflinkUnsupported is true for the errnos a filesystem returns when it has no FICLONE at all, never for a per-file or cross-device failure.
func reflinkUnsupported(err error) bool {
	return errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOTTY)
}
