package firecracker

import (
	"context"
	"slices"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/types"
)

// withSourceWritableDisksLocked locks the source VM's writable disks (COW + data) in sorted order so concurrent clones can't deadlock.
// Each acquire runs recoverStaleBackup to finish any interrupted prior swap.
// Lock files sit next to the disks and die with the VM dir; the fresh-inode
// window after a concurrent rm is closed by ensureSourceAlive under the lock.
func (*Firecracker) withSourceWritableDisksLocked(ctx context.Context, configs []*types.StorageConfig, fn func() error) error {
	paths := make([]string, 0, len(configs))
	for _, sc := range configs {
		if sc.Role == types.StorageRoleCOW || sc.Role == types.StorageRoleData {
			paths = append(paths, sc.Path)
		}
	}
	slices.Sort(paths)
	return withPathsLocked(ctx, paths, fn)
}

func withPathsLocked(ctx context.Context, paths []string, fn func() error) error {
	if len(paths) == 0 {
		return fn()
	}
	return withCOWPathLocked(ctx, paths[0], func() error {
		recoverStaleBackup(paths[0])
		return withPathsLocked(ctx, paths[1:], fn)
	})
}

func withCOWPathLocked(ctx context.Context, cowPath string, fn func() error) error {
	l := flock.New(cowPath + ".clone.lock")
	if lockErr := l.Lock(ctx); lockErr != nil {
		return lockErr
	}
	// Do NOT remove the lock file after unlock — flock synchronizes on
	// the inode, not the pathname.
	defer func() { _ = l.Unlock(ctx) }()

	return fn()
}
