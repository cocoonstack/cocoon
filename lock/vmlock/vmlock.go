// Package vmlock is the single resolver for per-VM operation locks: a
// stable, VMID-keyed, backend-independent path used by CH, FC and CNI alike
// (design §5). The lock lives outside every cleanup set — destructive
// teardown never deletes it — and derives from the vmID alone, so the CNI
// GC's recordless orphan netns can still find it with no record to consult.
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
