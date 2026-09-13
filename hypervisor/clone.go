package hypervisor

import (
	"context"
	"fmt"
	"io"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// CloneFilesFunc copies a snapshot's files from srcDir into dstDir.
type CloneFilesFunc func(dstDir, srcDir string) error

// AfterExtractFn finalizes a cloned VM after snapshot files are in place; rec is the placed placeholder, sourceSnapshotID flows through for metering lineage.
type AfterExtractFn func(ctx context.Context, rec *VMRecord, vmCfg *types.VMConfig, net types.NetSetup, sourceSnapshotID string) (*types.VM, error)

// CloneSpec carries one clone's inputs through the shared placeholder→populate→finalize skeleton.
type CloneSpec struct {
	VMCfg          *types.VMConfig
	Net            types.NetSetup
	SnapshotConfig *types.SnapshotConfig
	AfterExtract   AfterExtractFn
}

// DirectCloneBase clones from a local snapshot directory.
func (b *Backend) DirectCloneBase(ctx context.Context, vmID string, spec CloneSpec, srcDir string, cloneFiles CloneFilesFunc) (*types.VM, error) {
	return b.cloneBase(ctx, vmID, spec, func(runDir string) error {
		if err := cloneFiles(runDir, srcDir); err != nil {
			return fmt.Errorf("clone snapshot files: %w", err)
		}
		return nil
	})
}

// CloneFromStream clones from a tar stream into a fresh runDir.
func (b *Backend) CloneFromStream(ctx context.Context, vmID string, spec CloneSpec, snapshot io.Reader) (*types.VM, error) {
	return b.cloneBase(ctx, vmID, spec, func(runDir string) error {
		if err := utils.ExtractTar(runDir, snapshot, isLockFile); err != nil {
			return fmt.Errorf("extract snapshot: %w", err)
		}
		return nil
	})
}

func (b *Backend) RunningCloneRecord(rec *VMRecord, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, net types.NetSetup) *types.VM {
	now := timeNow()
	info := &types.VM{
		ID: rec.ID, Hypervisor: b.Typ, State: types.VMStateRunning,
		Config: *vmCfg, StorageConfigs: storageConfigs,
		CPUSet: rec.CPUSet, QueueCPUs: rec.QueueCPUs,
		NetSetup:  net,
		CreatedAt: rec.CreatedAt, UpdatedAt: now, StartedAt: &now,
	}
	SetRunningSockets(info, rec.RunDir)
	return info
}

// FinalizeClone persists the record and emits the clone open-interval pair.
func (b *Backend) FinalizeClone(ctx context.Context, vmID string, info *types.VM, bootCfg *types.BootConfig, blobIDs map[string]struct{}, sourceSnapshotID string) error {
	if err := b.UpdateRecord(ctx, vmID, func(r *VMRecord) error {
		r.VM = *info
		// The subpackage building info cannot reach markTransition; without this the clone commits at generation zero.
		markTransition(r, info.State, types.TransitionClone, timeNow())
		r.BootConfig = bootCfg
		r.FirstBooted = true
		if blobIDs != nil {
			r.ImageBlobIDs = blobIDs
		}
		return nil
	}); err != nil {
		return err
	}
	b.emitOpenInterval(ctx, info, metering.ReasonClone, sourceSnapshotID, timeNow())
	return nil
}

func (b *Backend) cloneBase(ctx context.Context, vmID string, spec CloneSpec, populate func(runDir string) error) (_ *types.VM, err error) {
	runDir, _, cleanup, err := b.reservePlaceholder(ctx, vmID, spec.VMCfg, spec.SnapshotConfig.ImageBlobIDs)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	rec, err := b.placeRecord(ctx, vmID, &spec.VMCfg.Config)
	if err != nil {
		return nil, fmt.Errorf("place VM: %w", err)
	}
	if err = populate(runDir); err != nil {
		return nil, err
	}
	return spec.AfterExtract(ctx, &rec, spec.VMCfg, spec.Net, spec.SnapshotConfig.ID)
}
