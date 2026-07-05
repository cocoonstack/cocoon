package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (b *Backend) KillForRestore(ctx context.Context, vmID string, rec *VMRecord, terminate func(pid int) error, runtimeFiles []string) error {
	killErr := b.WithRunningVM(ctx, rec, terminate)
	if killErr != nil && !errors.Is(killErr, ErrNotRunning) {
		b.MarkError(ctx, vmID)
		return fmt.Errorf("stop running VM: %w", killErr)
	}
	CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
	return nil
}

func (b *Backend) ResolveForRestore(ctx context.Context, vmRef string) (string, *VMRecord, error) {
	vmID, err := b.ResolveRef(ctx, vmRef)
	if err != nil {
		return "", nil, err
	}
	rec, err := b.LoadRecord(ctx, vmID)
	if err != nil {
		return "", nil, err
	}
	// Stopped restores too (hibernate resume): the sequence cold-spawns a fresh VMM either way and the kill step tolerates a dead one.
	if rec.State != types.VMStateRunning && rec.State != types.VMStateStopped {
		return "", nil, fmt.Errorf("vm %s is %s, must be running or stopped to restore", vmID, rec.State)
	}
	return vmID, &rec, nil
}

func (b *Backend) FinalizeRestore(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *VMRecord, pid int) (*types.VM, error) {
	now := time.Now()
	if err := b.DB.Update(ctx, func(idx *VMIndex) error {
		r, err := idx.GetRecord(vmID)
		if err != nil {
			return err
		}
		r.Config = *vmCfg
		r.State = types.VMStateRunning
		r.StartedAt = &now
		r.StoppedAt = nil
		r.UpdatedAt = now
		return nil
	}); err != nil {
		return nil, fmt.Errorf("update record: %w", err)
	}

	info := rec.VM
	info.Config = *vmCfg
	info.State = types.VMStateRunning
	info.PID = pid
	info.SocketPath = SocketPath(rec.RunDir)
	info.StartedAt = &now
	info.UpdatedAt = now
	return &info, nil
}

// RestoreSequence is the shared restore skeleton (preflight before kill).
func (b *Backend) RestoreSequence(ctx context.Context, vmRef string, spec RestoreSpec) (*types.VM, error) {
	if err := ValidateHostCPU(spec.VMCfg.CPU); err != nil {
		return nil, err
	}
	vmID, rec, err := b.ResolveForRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	stagingDir, cleanupStaging, err := PrepareStagingDir(rec.RunDir, spec.Snapshot)
	if err != nil {
		return nil, err
	}
	defer cleanupStaging()

	if preflightErr := spec.Preflight(stagingDir, rec); preflightErr != nil {
		return nil, fmt.Errorf("snapshot preflight: %w", preflightErr)
	}
	oldShape := shapeFromConfig(rec.Config)
	if killErr := spec.Kill(ctx, vmID, rec); killErr != nil {
		return nil, killErr
	}
	b.emitRestoreComputeStop(ctx, vmID, oldShape, spec.SourceSnapshotID)

	var result *types.VM
	inner := func() error {
		if spec.BeforeMerge != nil {
			if err := spec.BeforeMerge(rec); err != nil {
				return err
			}
		}
		if mergeErr := MergeDirInto(stagingDir, rec.RunDir); mergeErr != nil {
			b.MarkError(ctx, vmID)
			return fmt.Errorf("apply staged snapshot: %w", mergeErr)
		}
		var afterErr error
		result, afterErr = spec.AfterExtract(ctx, vmID, spec.VMCfg, rec)
		return afterErr
	}
	if spec.Wrap != nil {
		if err := spec.Wrap(rec, inner); err != nil {
			return nil, err
		}
	} else if err := inner(); err != nil {
		return nil, err
	}
	b.emitRestoreSuccess(ctx, result, oldShape, spec.SourceSnapshotID)
	return result, nil
}

// DirectRestoreSequence restores from a local snapshot directory.
func (b *Backend) DirectRestoreSequence(ctx context.Context, vmRef string, spec DirectRestoreSpec) (*types.VM, error) {
	if err := ValidateHostCPU(spec.VMCfg.CPU); err != nil {
		return nil, err
	}
	vmID, rec, err := b.ResolveForRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	if preflightErr := spec.Preflight(spec.SrcDir, rec); preflightErr != nil {
		return nil, fmt.Errorf("snapshot preflight: %w", preflightErr)
	}
	oldShape := shapeFromConfig(rec.Config)
	if killErr := spec.Kill(ctx, vmID, rec); killErr != nil {
		return nil, killErr
	}
	b.emitRestoreComputeStop(ctx, vmID, oldShape, spec.SourceSnapshotID)

	var result *types.VM
	inner := func() error {
		if populateErr := spec.Populate(rec, spec.SrcDir); populateErr != nil {
			b.MarkError(ctx, vmID)
			return populateErr
		}
		var afterErr error
		result, afterErr = spec.AfterExtract(ctx, vmID, spec.VMCfg, rec)
		return afterErr
	}
	if spec.Wrap != nil {
		if wrapErr := spec.Wrap(rec, inner); wrapErr != nil {
			return nil, wrapErr
		}
	} else if innerErr := inner(); innerErr != nil {
		return nil, innerErr
	}
	b.emitRestoreSuccess(ctx, result, oldShape, spec.SourceSnapshotID)
	return result, nil
}

// emitRestoreComputeStop closes the compute interval after a confirmed kill; fail-closed on DB error and skip on vanished record so the ledger never gets a phantom entry.
func (b *Backend) emitRestoreComputeStop(ctx context.Context, vmID string, oldShape metering.Shape, sourceSnapshotID string) {
	now := time.Now()
	closed := false
	if err := b.DB.Update(ctx, func(idx *VMIndex) error {
		r := idx.VMs[vmID]
		if r == nil || !hasOpenComputeInterval(r) {
			return nil
		}
		r.State = types.VMStateStopped
		r.StoppedAt = &now
		r.UpdatedAt = now
		closed = true
		return nil
	}); err != nil {
		log.WithFunc(b.Typ+".emitRestoreComputeStop").Warnf(ctx, "mark stopped after kill %s: %v", vmID, err)
		return
	}
	if !closed {
		return
	}
	b.Metering.Emit(ctx, b.makeSourceEntry(metering.KindVMComputeStop, vmID, sourceSnapshotID, metering.ReasonRestore, oldShape, now))
}

// emitRestoreSuccess closes old storage and opens fresh storage+compute.
func (b *Backend) emitRestoreSuccess(ctx context.Context, vm *types.VM, oldShape metering.Shape, sourceSnapshotID string) {
	now := time.Now()
	b.Metering.Emit(ctx, b.makeSourceEntry(metering.KindVMStorageStop, vm.ID, sourceSnapshotID, metering.ReasonRestore, oldShape, now))
	b.emitOpenInterval(ctx, vm, metering.ReasonRestore, sourceSnapshotID, now)
}

func PrepareStagingDir(runDir string, snapshot io.Reader) (stagingDir string, cleanup func(), err error) {
	stagingDir = runDir + ".restore-staging"
	if err = os.RemoveAll(stagingDir); err != nil {
		return "", nil, fmt.Errorf("clear staging dir: %w", err)
	}
	if err = os.MkdirAll(stagingDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}
	cleanup = func() { os.RemoveAll(stagingDir) } //nolint:errcheck,gosec
	if err = utils.ExtractTar(stagingDir, snapshot); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("extract snapshot: %w", err)
	}
	return stagingDir, cleanup, nil
}
