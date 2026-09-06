// Package vmlock resolves per-VM operation locks by vmID alone, so recordless consumers (orphan-netns GC, dead-source clone leases) derive the path without a record. Locks are transient: an exclusive release unlinks the file while still holding it, and every acquirer — including SharedLease — rebinds when it lands on an unlinked inode, so lease files self-clean instead of accumulating one per VM ever created.
package vmlock

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/utils"
)

const lockSuffix = ".lock"

func Path(rootDir, vmID string) string {
	return filepath.Join(lockDir(rootDir), vmID+lockSuffix)
}

// New returns vmID's operation lock, creating the lock directory on demand.
func New(rootDir, vmID string) (*flock.Lock, error) {
	p := Path(rootDir, vmID)
	if err := utils.EnsureDirs(filepath.Dir(p)); err != nil {
		return nil, err
	}
	return flock.NewTransient(p), nil
}

func lockDir(rootDir string) string {
	return filepath.Join(rootDir, "locks", "vm")
}
