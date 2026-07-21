package hypervisor

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// AfterExtractFn finalizes a cloned VM after snapshot files are in place; sourceSnapshotID flows through for metering lineage.
type AfterExtractFn func(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, runDir, logDir string, now time.Time, sourceSnapshotID string) (*types.VM, error)

// DirectCloneBase clones from a local snapshot directory. Used when the snapshot lives on the same host (no tar streaming needed).
func (b *Backend) DirectCloneBase(
	ctx context.Context, vmID string, vmCfg *types.VMConfig,
	net types.NetSetup, snapshotConfig *types.SnapshotConfig, srcDir string,
	cloneFiles func(dstDir, srcDir string) error,
	afterExtract AfterExtractFn,
) (*types.VM, error) {
	return b.cloneBase(ctx, vmID, vmCfg, net, snapshotConfig, afterExtract, func(runDir string) error {
		if err := cloneFiles(runDir, srcDir); err != nil {
			return fmt.Errorf("clone snapshot files: %w", err)
		}
		return nil
	})
}

// CloneFromStream clones from a tar stream into a fresh runDir. Used when the snapshot arrives over the network (cross-node clone).
func (b *Backend) CloneFromStream(
	ctx context.Context, vmID string, vmCfg *types.VMConfig,
	net types.NetSetup, snapshotConfig *types.SnapshotConfig, snapshot io.Reader,
	afterExtract AfterExtractFn,
) (*types.VM, error) {
	return b.cloneBase(ctx, vmID, vmCfg, net, snapshotConfig, afterExtract, func(runDir string) error {
		if err := utils.ExtractTar(runDir, snapshot, isLockFile); err != nil {
			return fmt.Errorf("extract snapshot: %w", err)
		}
		return nil
	})
}

func (b *Backend) cloneBase(
	ctx context.Context, vmID string, vmCfg *types.VMConfig,
	net types.NetSetup, snapshotConfig *types.SnapshotConfig,
	afterExtract AfterExtractFn, populate func(runDir string) error,
) (_ *types.VM, err error) {
	runDir, logDir, now, cleanup, err := b.reservePlaceholder(ctx, vmID, vmCfg, snapshotConfig.ImageBlobIDs)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	if err = populate(runDir); err != nil {
		return nil, err
	}
	return afterExtract(ctx, vmID, vmCfg, net, runDir, logDir, now, snapshotConfig.ID)
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
