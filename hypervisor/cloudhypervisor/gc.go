package cloudhypervisor

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type chSnapshot struct {
	blobIDs     map[string]struct{} // union of all VMs' ImageBlobIDs
	vmIDs       map[string]struct{} // all VM IDs in the DB
	staleCreate []string            // IDs in stale "creating" state (crash remnants)
	runDirs     []string            // subdirectory names under CHRunDir
	logDirs     []string            // subdirectory names under CHLogDir
}

func (s chSnapshot) UsedBlobIDs() map[string]struct{} { return s.blobIDs }
func (s chSnapshot) ActiveVMIDs() map[string]struct{} { return s.vmIDs }

// GCModule returns the GC module for cross-module blob pinning and orphan cleanup.
func (ch *CloudHypervisor) GCModule() gc.Module[chSnapshot] {
	return gc.Module[chSnapshot]{
		Name:   typ,
		Locker: ch.Locker,
		ReadDB: func(_ context.Context) (chSnapshot, error) {
			var snap chSnapshot
			cutoff := time.Now().Add(-hypervisor.CreatingStateGCGrace)
			if err := ch.DB.ReadRaw(func(idx *hypervisor.VMIndex) error {
				snap.blobIDs = make(map[string]struct{})
				snap.vmIDs = make(map[string]struct{})
				for id, rec := range idx.VMs {
					if rec == nil {
						continue
					}
					snap.vmIDs[id] = struct{}{}
					for hex := range rec.ImageBlobIDs {
						snap.blobIDs[hex] = struct{}{}
					}
					if rec.State == types.VMStateCreating && rec.UpdatedAt.Before(cutoff) {
						snap.staleCreate = append(snap.staleCreate, id)
					}
				}
				return nil
			}); err != nil {
				return snap, err
			}
			var err error
			if snap.runDirs, err = utils.ScanSubdirs(ch.conf.RunDir()); err != nil {
				return snap, err
			}
			if snap.logDirs, err = utils.ScanSubdirs(ch.conf.LogDir()); err != nil {
				return snap, err
			}
			return snap, nil
		},
		Resolve: func(snap chSnapshot, _ map[string]any) []string {
			// "db" is a reserved system subdirectory (stores vms.json/vms.lock).
			// When RootDir == RunDir, it lives alongside per-VM dirs and must be
			// excluded from orphan detection.
			reserved := map[string]struct{}{"db": {}}
			runOrphans := utils.FilterUnreferenced(snap.runDirs, snap.vmIDs, reserved)
			logOrphans := utils.FilterUnreferenced(snap.logDirs, snap.vmIDs, reserved)
			candidates := slices.Concat(runOrphans, logOrphans, snap.staleCreate)
			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		// Collect runs while the GC orchestrator holds the module's flock.
		// Use lock-free DB access (ReadRaw/WriteRaw) to avoid self-deadlock,
		// since the flock is shared between Backend.Locker and DB.locker.
		Collect: func(ctx context.Context, ids []string) error {
			var errs []error
			for _, id := range ids {
				runDir, logDir := ch.conf.VMRunDir(id), ch.conf.VMLogDir(id)
				_ = ch.DB.ReadRaw(func(idx *hypervisor.VMIndex) error {
					if rec := idx.VMs[id]; rec != nil {
						runDir, logDir = rec.RunDir, rec.LogDir
					}
					return nil
				})
				if err := hypervisor.RemoveVMDirs(runDir, logDir); err != nil {
					errs = append(errs, err)
				}
			}
			if err := ch.CleanStalePlaceholders(ctx, ids); err != nil {
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		},
	}
}

// RegisterGC registers the Cloud Hypervisor GC module with the given Orchestrator.
func (ch *CloudHypervisor) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, ch.GCModule())
}
