package hypervisor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (b *Backend) GracefulStop(ctx context.Context, vmID string, pid int, timeout time.Duration, signal, escalate func() error) error {
	logger := log.WithFunc(b.Typ + ".GracefulStop")
	if err := signal(); err != nil {
		logger.Warnf(ctx, "shutdown signal %s: %v — escalating", vmID, err)
		return escalate()
	}
	if err := utils.WaitProcessExit(ctx, pid, timeout); err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Warnf(ctx, "VM %s did not shut down within %s, escalating", vmID, timeout)
	return escalate()
}

// StopOneSequence stops id under its ops lock; a VMM that is down with its record or plumbing unsettled fails the stop, and a rerun settles it.
func (b *Backend) StopOneSequence(ctx context.Context, id string, spec StopSpec) error {
	unlock, err := b.LockVMOps(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	settleErr, err := b.stopOne(ctx, id, spec)
	return cmp.Or(err, settleErr)
}

// StopOneLocked skips the lock for DeleteAll, which already holds it and removes the record and the plumbing itself.
func (b *Backend) StopOneLocked(ctx context.Context, id string, spec StopSpec) error {
	settleErr, err := b.stopOne(ctx, id, spec)
	log.WithFunc(b.Typ+".StopOneLocked").Errorf(ctx, settleErr, "settle stopped VM %s before delete", id)
	return err
}

// StopAll stops each ref, each flipping its own state under its VM's ops lock.
func (b *Backend) StopAll(ctx context.Context, refs []string, spec StopSpec) ([]string, error) {
	ids, err := b.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	return b.ForEachVM(ctx, ids, "Stop", func(ctx context.Context, id string) error {
		return b.StopOneSequence(ctx, id, spec)
	})
}

// DeleteAll removes VMs by ref; each VM's stop+probe+delete runs under its ops lock (#103), so stopLocked must not re-take it.
func (b *Backend) DeleteAll(ctx context.Context, refs []string, force bool, stopLocked VMOp) ([]string, error) {
	ids, err := b.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	procScan, scanErr := utils.ScanProcsByBinary(b.Conf.BinaryName())
	if scanErr != nil {
		return nil, fmt.Errorf("refuse delete: /proc scan errored: %w (resolve the host issue and retry)", scanErr)
	}
	return b.ForEachVM(ctx, ids, "Delete", func(ctx context.Context, id string) error {
		unlock, lockErr := b.LockVMOps(ctx, id)
		if lockErr != nil {
			return lockErr
		}
		defer unlock()
		rec, loadErr := b.LoadRecord(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		return b.deleteOneLocked(ctx, id, force, stopLocked, &rec, procScan)
	})
}

func (b *Backend) stopOne(ctx context.Context, id string, spec StopSpec) (settleErr, err error) {
	rec, err := b.LoadRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.State == types.VMStateCreating {
		return nil, fmt.Errorf("vm %s is still being created", id)
	}
	sockPath := SocketPath(rec.RunDir)
	shutdownErr := b.WithRunningVM(ctx, &rec, func(pid int) error {
		return spec.Shutdown(ctx, &rec, sockPath, pid)
	})
	settled := errors.Is(shutdownErr, ErrNotRunning) && rec.State == types.VMStateStopped && !hasOpenComputeInterval(&rec)
	if shutdownErr != nil && !errors.Is(shutdownErr, ErrNotRunning) {
		b.MarkError(ctx, id)
		return nil, shutdownErr
	}
	return b.landStopped(ctx, id, &rec, spec.RuntimeFiles, settled), nil
}

// landStopped settles a VM whose VMM is gone, under the caller's ops lock; the quiesce runs even when the flip fails (#130).
func (b *Backend) landStopped(ctx context.Context, id string, rec *VMRecord, runtimeFiles []string, settled bool) error {
	CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
	b.removeCgroupScope(ctx, id)
	var flipErr error
	if !settled {
		if flipErr = b.UpdateState(ctx, id, types.VMStateStopped); flipErr != nil {
			flipErr = fmt.Errorf("mark stopped %s: %w", id, flipErr)
		}
	}
	return errors.Join(flipErr, b.quiesceAfterStop(ctx, id, rec, !settled, !settled && flipErr == nil))
}

func (b *Backend) deleteOneLocked(ctx context.Context, id string, force bool, stopLocked VMOp, rec *VMRecord, procScan utils.ProcScan) error {
	sockPath := SocketPath(rec.RunDir)
	stoppedByUs := false
	if runningErr := b.withRunningVM(ctx, rec, &procScan, func(_ int) error {
		if !force {
			return errors.New("running (force required)")
		}
		stoppedByUs = true
		if rec.State == types.VMStateCreating {
			return b.ensureOrphanVMMDead(ctx, rec.RunDir)
		}
		return stopLocked(ctx, id)
	}); runningErr != nil && !errors.Is(runningErr, ErrNotRunning) {
		return fmt.Errorf("stop before delete: %w", runningErr)
	}
	// Probe fires unconditionally: AF_UNIX has no TIME_WAIT, and catches false-negative pidfile/cmdline shortcuts.
	if live, probeErr := b.IsAPISocketLive(ctx, rec); live {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if probeErr != nil {
			return fmt.Errorf("refuse delete: api socket %s probe inconclusive: %w (resolve the host issue or kill the vmm process then retry)", sockPath, probeErr)
		}
		return fmt.Errorf("refuse delete: api socket %s still responsive (suspected orphan vmm; kill the vmm process then retry)", sockPath)
	}
	for _, pid := range procScan.Find(sockPath) {
		// procScan predates the stop: only a still-live match is a real orphan worth killing.
		if !utils.VerifyProcessCmdline(pid, sockPath) {
			continue
		}
		if termErr := utils.TerminateProcess(ctx, pid, sockPath, b.Conf.TerminateGracePeriod()); termErr != nil {
			return fmt.Errorf("terminate orphan VMM pid=%d for VM %s: %w", pid, id, termErr)
		}
		log.WithFunc(b.Typ+".deleteOneLocked").Warnf(ctx, "killed orphan VMM pid=%d for VM %s", pid, id)
	}
	shape := shapeFromConfig(rec.Config)
	hadRunningInterval := hasOpenComputeInterval(rec)
	if stoppedByUs {
		// The stop above already emitted its compute.stop; the pre-stop copy still reads open.
		if fresh, freshErr := b.LoadRecord(ctx, id); freshErr == nil {
			hadRunningInterval = hasOpenComputeInterval(&fresh)
		}
	}
	if err := b.deleteVMProtocol(ctx, id, rec); err != nil {
		return err
	}
	if rec.State == types.VMStateCreating {
		return nil
	}
	computeReason := metering.ReasonStopCrash
	if stoppedByUs {
		computeReason = metering.ReasonStopUser
	}
	b.emitDeleteClose(ctx, id, shape, computeReason, hadRunningInterval)
	return nil
}
