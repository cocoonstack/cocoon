package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (b *Backend) KillForRestore(ctx context.Context, vmID string, rec *VMRecord, terminate func(pid int) error, runtimeFiles []string) error {
	killErr := b.WithRunningVM(ctx, rec, terminate)
	if killErr != nil && !errors.Is(killErr, ErrNotRunning) {
		// A stopped origin has no live VMM: a transient liveness-scan error must not brick the wake — nothing was mutated, retry converges.
		b.failRestore(ctx, vmID, rec.State)
		return fmt.Errorf("stop running VM: %w", killErr)
	}
	CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
	return nil
}

// failRestore marks the VM error after a restore failure; a stopped origin (the pre-kill state) is spared so hibernate wake stays retryable — run-dir-mutating steps quarantine at their own site.
func (b *Backend) failRestore(ctx context.Context, vmID string, origin types.VMState) {
	if origin == types.VMStateStopped {
		return
	}
	b.MarkError(ctx, vmID)
}

func (b *Backend) ResolveForRestore(ctx context.Context, vmRef string) (string, *VMRecord, error) {
	vmID, rec, err := b.ResolveAndLoad(ctx, vmRef)
	if err != nil {
		return "", nil, err
	}
	if err := restorableState(vmID, &rec); err != nil {
		return "", nil, err
	}
	return vmID, &rec, nil
}

func (b *Backend) FinalizeRestore(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *VMRecord, pid int) (*types.VM, error) {
	now := timeNow()
	if err := b.UpdateRecord(ctx, vmID, func(r *VMRecord) error {
		r.Config = *vmCfg
		markTransition(r, types.VMStateRunning, types.TransitionRestore, now)
		r.Quarantine = ""
		r.StartedAt = &now
		r.StoppedAt = nil
		r.QuiescePending = false
		return nil
	}); err != nil {
		return nil, fmt.Errorf("update record: %w", err)
	}
	_ = os.Remove(filepath.Join(rec.RunDir, restoreDirtyName))

	info := rec.VM
	info.Config = *vmCfg
	info.State = types.VMStateRunning
	info.PID = pid
	SetRunningSockets(&info, rec.RunDir)
	info.StartedAt = &now
	info.UpdatedAt = now
	return &info, nil
}

// RestoreSequence is the shared restore skeleton (preflight before kill).
func (b *Backend) RestoreSequence(ctx context.Context, vmRef string, spec RestoreSpec) (*types.VM, error) {
	if err := ValidateHostCPU(spec.VMCfg.CPU); err != nil {
		return nil, err
	}
	vmID, rec, unlock, err := b.prepareRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}
	defer unlock()

	stagingDir, cleanupStaging, err := PrepareStagingDir(rec.RunDir, spec.Snapshot)
	if err != nil {
		return nil, err
	}
	defer cleanupStaging()

	if preflightErr := spec.Preflight(stagingDir, rec); preflightErr != nil {
		return nil, fmt.Errorf("snapshot preflight: %w", preflightErr)
	}
	apply := func(rec *VMRecord) error {
		if spec.BeforeMerge != nil {
			if err := spec.BeforeMerge(rec); err != nil {
				// The sweep may already have deleted snapshot files; a stopped origin would otherwise stay startable on mixed state.
				b.QuarantineVM(ctx, vmID, "partial restore cleanup")
				return err
			}
		}
		if mergeErr := MergeDirInto(stagingDir, rec.RunDir); mergeErr != nil {
			// A partial merge leaves mixed-vintage files in the run dir; quarantine regardless of origin so vm start cannot boot them.
			b.QuarantineVM(ctx, vmID, "partial snapshot merge")
			return fmt.Errorf("apply staged snapshot: %w", mergeErr)
		}
		return nil
	}
	return b.restoreCore(ctx, vmID, rec, spec.VMCfg, spec.SourceSnapshotID, spec.Kill, apply, spec.AfterExtract)
}

// DirectRestoreSequence restores from a local snapshot directory.
func (b *Backend) DirectRestoreSequence(ctx context.Context, vmRef string, spec DirectRestoreSpec) (*types.VM, error) {
	if err := ValidateHostCPU(spec.VMCfg.CPU); err != nil {
		return nil, err
	}
	vmID, rec, unlock, err := b.prepareRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if preflightErr := spec.Preflight(spec.SrcDir, rec); preflightErr != nil {
		return nil, fmt.Errorf("snapshot preflight: %w", preflightErr)
	}
	apply := func(rec *VMRecord) error {
		if populateErr := spec.Populate(rec, spec.SrcDir); populateErr != nil {
			// Populate cleans then clones with no rollback; a partial run dir must quarantine regardless of origin, like the merge.
			b.QuarantineVM(ctx, vmID, "partial restore populate")
			return populateErr
		}
		return nil
	}
	return b.restoreCore(ctx, vmID, rec, spec.VMCfg, spec.SourceSnapshotID, spec.Kill, apply, spec.AfterExtract)
}

// restoreCore is the shared kill→emit→apply→finalize tail of both restore sequences.
func (b *Backend) restoreCore(
	ctx context.Context, vmID string, rec *VMRecord, vmCfg *types.VMConfig, sourceSnapshotID string,
	kill func(ctx context.Context, vmID string, rec *VMRecord) error,
	apply func(*VMRecord) error,
	afterExtract func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *VMRecord) (*types.VM, error),
) (*types.VM, error) {
	oldShape := shapeFromConfig(rec.Config)
	if killErr := kill(ctx, vmID, rec); killErr != nil {
		return nil, killErr
	}
	b.emitRestoreComputeStop(ctx, vmID, oldShape, sourceSnapshotID)

	var result *types.VM
	inner := func() error {
		// Tombstone before the destructive phase: it survives lost quarantine writes and process death; only FinalizeRestore clears it.
		if err := markRestoreDirty(rec.RunDir); err != nil {
			return err
		}
		if err := apply(rec); err != nil {
			return err
		}
		// Inside the ops lock, like StartSequence: a hibernate resume after a host reboot finds no plumbing left to un-quiesce.
		if err := b.RecoverNetwork(ctx, rec); err != nil {
			return fmt.Errorf("recover network: %w", err)
		}
		var afterErr error
		result, afterErr = afterExtract(ctx, vmID, vmCfg, rec)
		return afterErr
	}
	if err := inner(); err != nil {
		// The kill already ran, so networking left up stays durable retry work even for a retryable stopped origin.
		b.markFailedOperation(ctx, vmID, rec.State != types.VMStateStopped)
		return nil, err
	}
	b.emitRestoreSuccess(ctx, result, oldShape, sourceSnapshotID)
	return result, nil
}

func (b *Backend) prepareRestore(ctx context.Context, vmRef string) (string, *VMRecord, func(), error) {
	vmID, _, err := b.ResolveForRestore(ctx, vmRef)
	if err != nil {
		return "", nil, nil, err
	}
	unlock, err := b.LockVMOps(ctx, vmID)
	if err != nil {
		return "", nil, nil, err
	}
	fail := func(err error) (string, *VMRecord, func(), error) {
		unlock()
		return "", nil, nil, err
	}
	// Revalidate under the lock (from the guard's own transaction): the pre-lock record may predate a concurrent mutating verb, and preflight anchors the external trust set to it.
	rec, err := b.EntryGuardLoad(ctx, vmID)
	if err != nil {
		return fail(err)
	}
	if sErr := restorableState(vmID, &rec); sErr != nil {
		return fail(sErr)
	}
	if vErr := types.ValidateStorageConfigs(rec.StorageConfigs); vErr != nil {
		return fail(fmt.Errorf("storage invariants violated: %w", vErr))
	}
	if vErr := types.ValidateNetworkConfigs(rec.NetworkConfigs); vErr != nil {
		return fail(fmt.Errorf("network invariants violated: %w", vErr))
	}
	return vmID, &rec, unlock, nil
}

// emitRestoreComputeStop closes the compute interval after a confirmed kill; fail-closed on DB error and skip on vanished record so the ledger never gets a phantom entry.
func (b *Backend) emitRestoreComputeStop(ctx context.Context, vmID string, oldShape metering.Shape, sourceSnapshotID string) {
	now := timeNow()
	closed := false
	if err := b.update(ctx, func(t *vmTx) error {
		closed = false
		r, err := t.Get(vmID)
		if err != nil {
			return err
		}
		if r == nil || !hasOpenComputeInterval(r) {
			return nil
		}
		markTransition(r, types.VMStateStopped, types.TransitionRestore, now)
		r.StoppedAt = &now
		closed = true
		return t.Put(vmID, r)
	}); err != nil {
		log.WithFunc(b.Typ+".emitRestoreComputeStop").Warnf(ctx, "mark stopped after kill %s: %v", vmID, err)
		return
	}
	if !closed {
		return
	}
	b.Metering.Emit(ctx, b.makeSourceEntry(metering.KindVMComputeStop, vmID, sourceSnapshotID, metering.ReasonRestore, oldShape, now))
}

func (b *Backend) emitRestoreSuccess(ctx context.Context, vm *types.VM, oldShape metering.Shape, sourceSnapshotID string) {
	now := timeNow()
	b.Metering.Emit(ctx, b.makeSourceEntry(metering.KindVMStorageStop, vm.ID, sourceSnapshotID, metering.ReasonRestore, oldShape, now))
	b.emitOpenInterval(ctx, vm, metering.ReasonRestore, sourceSnapshotID, now)
}

// PrepareStagingDir extracts the snapshot into a staging dir inside the run dir: a top-level sibling would look like an orphan to the GC run-dir scan and be reaped mid-restore.
func PrepareStagingDir(runDir string, snapshot io.Reader) (stagingDir string, cleanup func(), err error) {
	stagingDir = filepath.Join(runDir, restoreStagingName)
	if err = os.RemoveAll(stagingDir); err != nil {
		return "", nil, fmt.Errorf("clear staging dir: %w", err)
	}
	if err = os.MkdirAll(stagingDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}
	cleanup = func() { os.RemoveAll(stagingDir) } //nolint:errcheck,gosec
	if err = utils.ExtractTar(stagingDir, snapshot, isLockFile); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("extract snapshot: %w", err)
	}
	return stagingDir, cleanup, nil
}

// restorableState allows Running, Stopped and Error origins. Stopped restores too (hibernate resume): the sequence cold-spawns a fresh VMM either way and the kill step tolerates a dead one. Error VMs are allowed through (quarantine sets Error too) — restore rebuilds the run dir, so it is the recovery path start.go redirects a crashed restore to; failRestore leaves a running-origin failure in Error with no quarantine reason, and rejecting it here would dead-end at vm rm.
func restorableState(vmID string, rec *VMRecord) error {
	if rec.State != types.VMStateRunning && rec.State != types.VMStateStopped && rec.State != types.VMStateError {
		return fmt.Errorf("vm %s is %s and cannot be restored", vmID, rec.State)
	}
	return nil
}

func markRestoreDirty(runDir string) error {
	if err := os.WriteFile(filepath.Join(runDir, restoreDirtyName), nil, 0o600); err != nil {
		return fmt.Errorf("mark restore dirty: %w", err)
	}
	return nil
}
