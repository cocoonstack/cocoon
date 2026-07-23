//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package vmlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/utils"
)

const sharedLeaseRetryDelay = 2 * time.Millisecond

// SharedLease holds a shared VM-operation flock and exposes its file for inheritance by a child process.
type SharedLease struct {
	file *os.File
}

// NewSharedLease acquires vmID's shared operation lease, rebinding whenever a transient exclusive release unlinked the inode it acquired.
func NewSharedLease(ctx context.Context, rootDir, vmID string) (*SharedLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire shared VM lease: %w", err)
	}
	path := Path(rootDir, vmID)
	if err := utils.EnsureDirs(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("create VM lease directory: %w", err)
	}
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // VM lock path under the configured root
		if err != nil {
			return nil, fmt.Errorf("open VM lease %s: %w", path, err)
		}
		lease, err := flockSharedBound(ctx, f, path)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("acquire shared VM lease %s: %w", path, err)
		}
		if lease != nil {
			return lease, nil
		}
		_ = f.Close()
	}
}

// File returns the locked file to pass through exec.Cmd.ExtraFiles.
func (l *SharedLease) File() *os.File { return l.file }

// Close drops this descriptor. The flock is released when the last descriptor inherited by any child closes.
func (l *SharedLease) Close() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// flockSharedBound holds LOCK_SH on f once it is the inode bound to path; nil,nil means the inode went stale and the caller must reopen.
func flockSharedBound(ctx context.Context, f *os.File, path string) (*SharedLease, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		switch {
		case err == nil:
			held, statErr := f.Stat()
			if statErr != nil {
				return nil, statErr
			}
			if flock.BoundToPath(held, path) {
				return &SharedLease{file: f}, nil
			}
			return nil, nil
		case !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN):
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sharedLeaseRetryDelay):
		}
	}
}
