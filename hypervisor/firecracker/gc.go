package firecracker

import (
	"context"
	"slices"
	"time"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type fcSnapshot struct {
	blobIDs     map[string]struct{}
	vmIDs       map[string]struct{}
	staleCreate []string
	runDirs     []string
	logDirs     []string
}

// UsedBlobIDs returns blob IDs referenced by all Firecracker VMs.
func (s fcSnapshot) UsedBlobIDs() map[string]struct{} { return s.blobIDs }

// ActiveVMIDs returns IDs of all Firecracker VMs in the database.
func (s fcSnapshot) ActiveVMIDs() map[string]struct{} { return s.vmIDs }

// GCModule returns the GC module for cross-module blob pinning and orphan cleanup.
func (fc *Firecracker) GCModule() gc.Module[fcSnapshot] {
	return gc.Module[fcSnapshot]{
		Name:   typ,
		Locker: fc.Locker,
		ReadDB: func(_ context.Context) (fcSnapshot, error) {
			var snap fcSnapshot
			cutoff := time.Now().Add(-hypervisor.CreatingStateGCGrace)
			if err := fc.DB.ReadRaw(func(idx *hypervisor.VMIndex) error {
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
			if snap.runDirs, err = utils.ScanSubdirs(fc.conf.RunDir()); err != nil {
				return snap, err
			}
			if snap.logDirs, err = utils.ScanSubdirs(fc.conf.LogDir()); err != nil {
				return snap, err
			}
			return snap, nil
		},
		Resolve: func(snap fcSnapshot, _ map[string]any) []string {
			reserved := map[string]struct{}{"db": {}}
			runOrphans := utils.FilterUnreferenced(snap.runDirs, snap.vmIDs, reserved)
			logOrphans := utils.FilterUnreferenced(snap.logDirs, snap.vmIDs, reserved)
			candidates := slices.Concat(runOrphans, logOrphans, snap.staleCreate)
			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		Collect: fc.GCCollect,
	}
}

// RegisterGC registers the Firecracker GC module with the given Orchestrator.
func (fc *Firecracker) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, fc.GCModule())
}
