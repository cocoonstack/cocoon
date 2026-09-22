package flock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// SharedLease holds a shared flock and exposes its file for inheritance by a child process.
type SharedLease struct {
	file *os.File
}

// AcquireShared blocks until path's shared lock is held, rebinding whenever a transient exclusive release unlinked the inode it acquired.
func AcquireShared(ctx context.Context, path string) (*SharedLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire shared lease %s: %w", path, err)
	}
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lock path built by the caller from a validated root
		if err != nil {
			return nil, fmt.Errorf("open lease %s: %w", path, err)
		}
		lease, err := sharedBound(ctx, f, path)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("acquire shared lease %s: %w", path, err)
		}
		if lease != nil {
			return lease, nil
		}
		_ = f.Close()
	}
}

// File returns the locked file to pass through exec.Cmd.ExtraFiles.
func (l *SharedLease) File() *os.File { return l.file }

// Close drops this descriptor; the flock releases only when every inherited copy closes.
func (l *SharedLease) Close() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// sharedBound holds LOCK_SH on f once it is the inode bound to path; nil,nil means the inode went stale and the caller must reopen.
func sharedBound(ctx context.Context, f *os.File, path string) (*SharedLease, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		switch {
		case err == nil:
			held, statErr := f.Stat()
			if statErr != nil {
				return nil, statErr
			}
			if BoundToPath(held, path) {
				return &SharedLease{file: f}, nil
			}
			return nil, nil
		case !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN):
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryDelay):
		}
	}
}
