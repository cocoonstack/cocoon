package vmlock

import (
	"context"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/utils"
)

type lockSnapshot struct {
	ids []string
}

// GCModule sweeps lease files for VMs no backend knows anymore.
func GCModule(rootDir string) gc.Module[lockSnapshot] {
	return gc.Module[lockSnapshot]{
		Name: "vmlock",
		ReadDB: func(context.Context) (lockSnapshot, error) {
			ids, err := utils.ScanFileStems(lockDir(rootDir), lockSuffix)
			return lockSnapshot{ids: ids}, err
		},
		Resolve: func(_ context.Context, snap lockSnapshot, others map[string]any) []string {
			return utils.FilterUnreferenced(snap.ids, gc.Collect(others, gc.VMIDs))
		},
		Collect: func(ctx context.Context, ids []string, _ lockSnapshot) error {
			logger := log.WithFunc("gc.vmlock")
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
