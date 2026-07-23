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

	"github.com/cocoonstack/cocoon/utils"
)

const sharedLeaseRetryDelay = 2 * time.Millisecond

// SharedLease holds a shared VM-operation flock and exposes its file for inheritance by a child process.
type SharedLease struct {
	file *os.File
}

// NewSharedLease acquires vmID's shared operation lease.
func NewSharedLease(ctx context.Context, rootDir, vmID string) (*SharedLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire shared VM lease: %w", err)
	}
	path := Path(rootDir, vmID)
	if err := utils.EnsureDirs(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("create VM lease directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // VM lock path under the configured root
	if err != nil {
		return nil, fmt.Errorf("open VM lease %s: %w", path, err)
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		switch {
		case err == nil:
			return &SharedLease{file: f}, nil
		case !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN):
			_ = f.Close()
			return nil, fmt.Errorf("acquire shared VM lease %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("acquire shared VM lease %s: %w", path, ctx.Err())
		case <-time.After(sharedLeaseRetryDelay):
		}
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
