package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/types"
)

// withSourceWritableDisksLocked locks the source's writable disks in sorted order (deadlock-free), self-healing interrupted swaps on acquire.
func (fc *Firecracker) withSourceWritableDisksLocked(ctx context.Context, configs []*types.StorageConfig, fn func() error) error {
	paths := make([]string, 0, len(configs))
	for _, sc := range configs {
		if sc.Role == types.StorageRoleCOW || sc.Role == types.StorageRoleData {
			paths = append(paths, sc.Path)
		}
	}
	if len(paths) == 0 {
		return fn()
	}
	slices.Sort(paths)
	// Dedupe adjacent equal paths: a duplicate would recurse into the same
	// lock twice from one goroutine, and the transient flock polls forever
	// against a lock this goroutine already holds — a self-deadlock.
	paths = slices.Compact(paths)
	lockDir := fc.cloneLockDir()
	if mkErr := os.MkdirAll(lockDir, 0o700); mkErr != nil {
		return fmt.Errorf("create clone lock dir: %w", mkErr)
	}
	return withPathsLocked(ctx, fc.Conf.RunDir(), lockDir, paths, fn)
}

func (fc *Firecracker) cloneLockDir() string {
	return filepath.Join(fc.Conf.RunDir(), hypervisor.CloneLocksDirName)
}

func withPathsLocked(ctx context.Context, runRoot, lockDir string, paths []string, fn func() error) error {
	if len(paths) == 0 {
		return fn()
	}
	return withCOWPathLocked(ctx, lockDir, paths[0], func() error {
		recoverStaleBackup(runRoot, paths[0])
		return withPathsLocked(ctx, runRoot, lockDir, paths[1:], fn)
	})
}

func withCOWPathLocked(ctx context.Context, lockDir, cowPath string, fn func() error) error {
	sum := sha256.Sum256([]byte(cowPath))
	l := flock.NewTransient(filepath.Join(lockDir, hex.EncodeToString(sum[:8])+"-"+filepath.Base(cowPath)+".clone.lock"))
	if lockErr := l.Lock(ctx); lockErr != nil {
		return lockErr
	}
	defer func() { _ = l.Unlock(ctx) }()

	return fn()
}
