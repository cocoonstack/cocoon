// Package vmlock resolves per-VM operation locks by vmID alone, so recordless orphan-netns GC can still find them; the lock file is never deleted by cleanup.
package vmlock

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocoonstack/cocoon/lock/flock"
)

// Path returns the lock file path for vmID under rootDir.
func Path(rootDir, vmID string) string {
	return filepath.Join(rootDir, "locks", "vm", vmID+".lock")
}

// New returns vmID's operation lock, creating the lock directory on demand.
func New(rootDir, vmID string) (*flock.Lock, error) {
	p := Path(rootDir, vmID)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return nil, fmt.Errorf("ensure vm lock dir: %w", err)
	}
	return flock.New(p), nil
}
