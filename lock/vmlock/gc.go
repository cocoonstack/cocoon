package vmlock

import (
	"context"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/utils"
)

// GCModule sweeps lease files for VMs no backend knows anymore.
func GCModule(rootDir string) gc.Module[[]string] {
	return gc.Module[[]string]{
		Name: "vmlock",
		ReadDB: func(context.Context) ([]string, error) {
			return utils.ScanFileStems(lockDir(rootDir), lockSuffix)
		},
		Resolve: func(_ context.Context, ids []string, others map[string]any) []string {
			return utils.FilterUnreferenced(ids, gc.Collect(others, gc.VMIDs))
		},
		Collect: func(ctx context.Context, ids, _ []string) error {
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
