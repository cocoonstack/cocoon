package flock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/gofrs/flock"
)

const retryDelay = 2 * time.Millisecond

// Lock provides in-process (channel) + cross-process (flock) mutual exclusion.
type Lock struct {
	path      string
	ch        chan struct{}
	fl        *flock.Flock // active flock fd, non-nil while held
	transient bool
}

// New returns a Lock guarding path; the lock file is created on first acquire.
func New(path string) *Lock {
	return &Lock{path: path, ch: make(chan struct{}, 1)}
}

// NewTransient returns a Lock that unlinks its file on Unlock; Lock retries if it acquires an already-unlinked inode instead of splitting the lock.
func NewTransient(path string) *Lock {
	return &Lock{path: path, ch: make(chan struct{}, 1), transient: true}
}

func (l *Lock) Lock(ctx context.Context) error {
	select {
	case l.ch <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("acquire lock %s: %w", l.path, ctx.Err())
	}
	for {
		ok, err := l.commitFlock(func(fl *flock.Flock) (bool, error) {
			return fl.TryLockContext(ctx, retryDelay)
		})
		if err != nil {
			return fmt.Errorf("acquire flock %s: %w", l.path, err)
		}
		if !ok {
			return fmt.Errorf("acquire flock %s: %w", l.path, ctx.Err())
		}
		if l.pathBound() {
			return nil
		}
		l.dropFlock()
	}
}

func (l *Lock) TryLock(_ context.Context) (bool, error) {
	select {
	case l.ch <- struct{}{}:
	default:
		return false, nil
	}
	ok, err := l.commitFlock(func(fl *flock.Flock) (bool, error) {
		return fl.TryLock()
	})
	if err != nil || !ok {
		return ok, err
	}
	if !l.pathBound() {
		l.dropFlock()
		<-l.ch
		return false, nil
	}
	return true, nil
}

func (l *Lock) Unlock(_ context.Context) error {
	var err error
	if l.fl != nil {
		// Unlink before funlock: still holding a verified lock, so no waiter can win the pathname until it rebinds to a fresh inode.
		if l.transient {
			if rmErr := os.Remove(l.path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				err = rmErr
			}
		}
		err = errors.Join(err, l.fl.Unlock())
		l.fl = nil
	}
	select {
	case <-l.ch:
	default:
	}
	if err != nil {
		return fmt.Errorf("release flock %s: %w", l.path, err)
	}
	return nil
}

// commitFlock acquires a flock fd; on failure releases the channel token so Lock/Unlock stay balanced.
func (l *Lock) commitFlock(acquire func(*flock.Flock) (bool, error)) (bool, error) {
	fl := flock.New(l.path)
	locked, err := acquire(fl)
	if err != nil || !locked {
		_ = fl.Close()
		<-l.ch
		return false, err
	}
	l.fl = fl
	return true, nil
}

// pathBound reports whether the held fd is still the inode bound to path; false means the file was unlinked (and possibly recreated) while waiting.
func (l *Lock) pathBound() bool {
	if !l.transient {
		return true
	}
	held, err := l.fl.Stat()
	if err != nil {
		return false
	}
	cur, err := os.Stat(l.path)
	return err == nil && os.SameFile(held, cur)
}

// dropFlock releases the kernel lock while keeping the in-process token, for requeueing on a fresh inode.
func (l *Lock) dropFlock() {
	_ = l.fl.Close()
	l.fl = nil
}
