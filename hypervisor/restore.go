package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type restoreRun struct {
	vmID             string
	rec              *VMRecord
	vmCfg            *types.VMConfig
	sourceSnapshotID string
	kill             KillHook
	apply            func(*VMRecord) error
	afterExtract     AfterExtractHook
}

func (b *Backend) KillForRestore(ctx context.Context, vmID string, rec *VMRecord, terminate func(pid int) error, runtimeFiles []string) error {
	killErr := b.WithRunningVM(ctx, rec, terminate)
	if killErr != nil && !errors.Is(killErr, ErrNotRunning) {
		b.failRestore(ctx, vmID, rec.State)
		return fmt.Errorf("stop running VM: %w", killErr)
	}
	CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
	return nil
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
	if rmErr := os.Remove(filepath.Join(rec.RunDir, restoreDirtyName)); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		log.WithFunc(b.Typ+".FinalizeRestore").Warnf(ctx, "clear restore-dirty marker for %s: %v; the next start clears it", vmID, rmErr)
	}

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
	vmID, rec, unlock, err := b.prepareRestore(ctx, vmRef, spec.VMCfg)
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
		if err := spec.Clean(rec.RunDir); err != nil {
			b.QuarantineVM(ctx, vmID, "partial restore cleanup")
			return err
		}
		if mergeErr := MergeDirInto(stagingDir, rec.RunDir); mergeErr != nil {
			b.QuarantineVM(ctx, vmID, "partial snapshot merge")
			return fmt.Errorf("apply staged snapshot: %w", mergeErr)
		}
		return nil
	}
	return b.restoreCore(ctx, restoreRun{vmID: vmID, rec: rec, vmCfg: spec.VMCfg, sourceSnapshotID: spec.SourceSnapshotID, kill: spec.Kill, apply: apply, afterExtract: spec.AfterExtract})
}

// DirectRestoreSequence restores from a local snapshot directory.
func (b *Backend) DirectRestoreSequence(ctx context.Context, vmRef string, spec DirectRestoreSpec) (*types.VM, error) {
	vmID, rec, unlock, err := b.prepareRestore(ctx, vmRef, spec.VMCfg)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if preflightErr := spec.Preflight(spec.SrcDir, rec); preflightErr != nil {
		return nil, fmt.Errorf("snapshot preflight: %w", preflightErr)
	}
	apply := func(rec *VMRecord) error {
		if populateErr := spec.Populate(rec, spec.SrcDir); populateErr != nil {
			// Populate has no rollback; quarantine any origin on a partial run dir.
			b.QuarantineVM(ctx, vmID, "partial restore populate")
			return populateErr
		}
		return nil
	}
	return b.restoreCore(ctx, restoreRun{vmID: vmID, rec: rec, vmCfg: spec.VMCfg, sourceSnapshotID: spec.SourceSnapshotID, kill: spec.Kill, apply: apply, afterExtract: spec.AfterExtract})
}

// failRestore spares a stopped origin so hibernate wake stays retryable.
func (b *Backend) failRestore(ctx context.Context, vmID string, origin types.VMState) {
	if origin == types.VMStateStopped {
		return
	}
	b.MarkError(ctx, vmID)
}

func (b *Backend) restoreCore(ctx context.Context, run restoreRun) (*types.VM, error) {
	oldShape := shapeFromConfig(run.rec.Config)
	if killErr := run.kill(ctx, run.vmID, run.rec); killErr != nil {
		return nil, killErr
	}
	b.emitRestoreComputeStop(ctx, run.vmID, oldShape, run.sourceSnapshotID)

	var result *types.VM
	inner := func() error {
		if err := os.WriteFile(filepath.Join(run.rec.RunDir, restoreDirtyName), nil, 0o600); err != nil {
			return fmt.Errorf("mark restore dirty: %w", err)
		}
		if err := run.apply(run.rec); err != nil {
			return err
		}
		// Inside the ops lock, like StartSequence: a hibernate resume after a host reboot finds no plumbing left to un-quiesce.
		if err := b.RecoverNetwork(ctx, run.rec); err != nil {
			return fmt.Errorf("recover network: %w", err)
		}
		placed, err := b.placeRecord(ctx, run.vmID, &run.vmCfg.Config)
		if err != nil {
			return fmt.Errorf("place VM: %w", err)
		}
		run.rec.CPUSet, run.rec.QueueCPUs = placed.CPUSet, placed.QueueCPUs
		var afterErr error
		result, afterErr = run.afterExtract(ctx, run.vmID, run.vmCfg, run.rec)
		return afterErr
	}
	if err := inner(); err != nil {
		// The kill already ran, so networking left up stays durable retry work even for a retryable stopped origin.
		b.markFailedOperation(ctx, run.vmID, run.rec.State != types.VMStateStopped)
		return nil, err
	}
	now := timeNow()
	b.Metering.Emit(ctx, b.makeSourceEntry(metering.KindVMStorageStop, result.ID, run.sourceSnapshotID, metering.ReasonRestore, oldShape, now))
	b.emitOpenInterval(ctx, result, metering.ReasonRestore, run.sourceSnapshotID, now)
	return result, nil
}

func (b *Backend) prepareRestore(ctx context.Context, vmRef string, vmCfg *types.VMConfig) (string, *VMRecord, func(), error) {
	if err := ValidateHostCPU(vmCfg.CPU); err != nil {
		return "", nil, nil, err
	}
	vmID, err := b.ResolveRef(ctx, vmRef)
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
	// Revalidate under the lock; the pre-lock record may predate a concurrent mutating verb.
	rec, err := b.EntryGuardLoad(ctx, vmID)
	if err != nil {
		return fail(err)
	}
	if sErr := restorableState(vmID, &rec); sErr != nil {
		return fail(sErr)
	}
	if vErr := validateRecordInvariants(&rec); vErr != nil {
		return fail(vErr)
	}
	if !sameImage(vmCfg, &rec.Config) {
		return fail(fmt.Errorf("snapshot base image differs from vm %s; use vm clone instead", vmID))
	}
	return vmID, &rec, unlock, nil
}

// emitRestoreComputeStop closes the compute interval after a confirmed kill; a vanished record gets no phantom entry.
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
		r.QuiescePending = needsQuiesce(r)
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

// PrepareStagingDir extracts inside the run dir; a top-level sibling would look like an orphan to the GC run-dir scan.
func PrepareStagingDir(runDir string, snapshot io.Reader) (stagingDir string, cleanup func(), err error) {
	stagingDir = filepath.Join(runDir, restoreStagingName)
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

// Error is restorable: restore is the recovery path out of a crashed restore.
func restorableState(vmID string, rec *VMRecord) error {
	if rec.State != types.VMStateRunning && rec.State != types.VMStateStopped && rec.State != types.VMStateError {
		return fmt.Errorf("vm %s is %s and cannot be restored", vmID, rec.State)
	}
	return nil
}

func sameImage(a, b *types.VMConfig) bool {
	if a.ImageType != b.ImageType {
		return false
	}
	if a.ImageDigest != "" && b.ImageDigest != "" {
		return a.ImageDigest == b.ImageDigest
	}
	return a.Image == b.Image
}
