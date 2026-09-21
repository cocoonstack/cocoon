package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/utils"
)

// CgroupGCModule removes empty scopes owned by no VM in any backend snapshot; it never kills scope members.
func CgroupGCModule(parentDir string, vmInUse network.VMInUse) gc.Module[[]string] {
	return gc.Module[[]string]{
		Name: "cgroup",
		ReadDB: func(context.Context) ([]string, error) {
			return cgroup.ListScopeVMIDs(parentDir)
		},
		Resolve: func(_ context.Context, scopes []string, others map[string]any) []string {
			return utils.FilterUnreferenced(scopes, gc.Collect(others, gc.VMIDs))
		},
		Collect: func(ctx context.Context, ids, _ []string) error {
			logger := log.WithFunc("gc.cgroup")
			var errs []error
			for _, id := range ids {
				if inUse, err := vmInUse(ctx, id); err != nil {
					errs = append(errs, fmt.Errorf("owner of scope vm-%s: %w", id, err))
					continue
				} else if inUse {
					continue
				}
				// a scope populated since the VM snapshot was read belongs to a launch in flight, not to an orphan
				err := cgroup.RemoveEmpty(parentDir, id)
				if errors.Is(err, syscall.EBUSY) {
					logger.Infof(ctx, "skipped scope vm-%s reason=populated", id)
					continue
				}
				if err != nil {
					errs = append(errs, err)
					continue
				}
				logger.Infof(ctx, "collected scope vm-%s reason=orphan-scope", id)
			}
			return errors.Join(errs...)
		},
	}
}

// removeCgroupScope reclaims id's scope after its VMM is confirmed dead; failure is left for the next converge/GC pass.
func (b *Backend) removeCgroupScope(ctx context.Context, id string) {
	if err := cgroup.Remove(ctx, b.Conf.CgroupParentDir(), id); err != nil {
		log.WithFunc(b.Typ+".removeCgroupScope").Warnf(ctx, "remove scope for %s: %v (left for gc)", id, err)
	}
}
