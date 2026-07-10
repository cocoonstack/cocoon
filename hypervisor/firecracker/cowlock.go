package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/types"
)

// withSourceWritableDisksLocked locks the source VM's writable disks (COW + data) in sorted order so concurrent clones can't deadlock.
// Each acquire runs recoverStaleBackup to finish any interrupted prior swap.
// Lock files live in a stable directory outside the VM dirs: a lock unlinked
// by rm/GC while held would let a waiter acquire a fresh inode alongside it.
func (fc *Firecracker) withSourceWritableDisksLocked(ctx context.Context, configs []*types.StorageConfig, fn func() error) error {
	paths := make([]string, 0, len(configs))
	for _, sc := range configs {
		if sc.Role == types.StorageRoleCOW || sc.Role == types.StorageRoleData {
			paths = append(paths, sc.Path)
		}
	}
	slices.Sort(paths)
	return withPathsLocked(ctx, fc.cloneLockDir(), paths, fn)
}

func (fc *Firecracker) cloneLockDir() string {
	return filepath.Join(fc.Conf.RunDir(), "clone-locks")
}

func withPathsLocked(ctx context.Context, lockDir string, paths []string, fn func() error) error {
	if len(paths) == 0 {
		return fn()
	}
	return withCOWPathLocked(ctx, lockDir, paths[0], func() error {
		recoverStaleBackup(paths[0])
		return withPathsLocked(ctx, lockDir, paths[1:], fn)
	})
}

func withCOWPathLocked(ctx context.Context, lockDir, cowPath string, fn func() error) error {
	if mkErr := os.MkdirAll(lockDir, 0o700); mkErr != nil {
		return fmt.Errorf("create clone lock dir: %w", mkErr)
	}
	sum := sha256.Sum256([]byte(cowPath))
	l := flock.New(filepath.Join(lockDir, hex.EncodeToString(sum[:8])+"-"+filepath.Base(cowPath)+".clone.lock"))
	if lockErr := l.Lock(ctx); lockErr != nil {
		return lockErr
	}
	// Do NOT remove the lock file after unlock — flock synchronizes on
	// the inode, not the pathname.
	defer func() { _ = l.Unlock(ctx) }()

	return fn()
}
