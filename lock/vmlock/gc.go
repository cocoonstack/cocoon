package vmlock

import (
	"context"
	"os"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/utils"
)

type lockSnapshot struct {
	ids []string
}

// GCModule sweeps lease files for VMs no backend knows anymore; a held lock loses the exclusive TryLock and survives, and an acquirer racing the unlink rebinds to a fresh inode.
func GCModule(rootDir string) gc.Module[lockSnapshot] {
	return gc.Module[lockSnapshot]{
		Name: "vmlock",
		ReadDB: func(context.Context) (lockSnapshot, error) {
			entries, err := os.ReadDir(lockDir(rootDir))
			if err != nil {
				if os.IsNotExist(err) {
					return lockSnapshot{}, nil
				}
				return lockSnapshot{}, err
			}
			var snap lockSnapshot
			for _, e := range entries {
				if name, ok := strings.CutSuffix(e.Name(), lockSuffix); ok && e.Type().IsRegular() {
					snap.ids = append(snap.ids, name)
				}
			}
			return snap, nil
		},
		Resolve: func(_ context.Context, snap lockSnapshot, others map[string]any) []string {
			return utils.FilterUnreferenced(snap.ids, gc.Collect(others, gc.VMIDs))
		},
		Collect: func(ctx context.Context, ids []string, _ lockSnapshot) error {
			logger := log.WithFunc("vmlock.GCModule")
			for _, id := range ids {
				ok, err := flock.ReclaimTransient(ctx, Path(rootDir, id))
				if err != nil {
					logger.Warnf(ctx, "sweep lease %s: %v", id, err)
					continue
				}
				if ok {
					logger.Infof(ctx, "collected id=%s reason=orphan-lease", id)
				}
			}
			return nil
		},
	}
}
