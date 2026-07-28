package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// GracefulStop signals shutdown, polls until exit, escalates on timeout.
func (b *Backend) GracefulStop(ctx context.Context, vmID string, pid int, timeout time.Duration, signal, escalate func() error) error {
	logger := log.WithFunc(b.Typ + ".GracefulStop")
	if err := signal(); err != nil {
		logger.Warnf(ctx, "shutdown signal %s: %v — escalating", vmID, err)
		return escalate()
	}
	if err := utils.WaitFor(ctx, timeout, GracefulStopPollInterval, func() (bool, error) {
		return !utils.IsProcessAlive(pid), nil
	}); err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Warnf(ctx, "VM %s did not shut down within %s, escalating", vmID, timeout)
	return escalate()
}

// StopOneSequence runs the shared per-id stop skeleton (LoadRecord → WithRunningVM(Shutdown) → HandleStopResult) under the VM's ops lock so backends only express their force-vs-graceful choice.
func (b *Backend) StopOneSequence(ctx context.Context, id string, spec StopSpec) error {
	unlock, err := b.LockVMOps(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return b.StopOneLocked(ctx, id, spec)
}

// StopOneLocked is StopOneSequence minus the lock; DeleteAll calls it with the VM's ops lock already held (re-locking would deadlock). The Stopped flip lands inside the caller's lock so a start queued behind this stop can't interleave between the kill and the state write.
func (b *Backend) StopOneLocked(ctx context.Context, id string, spec StopSpec) error {
	rec, err := b.LoadRecord(ctx, id)
	if err != nil {
		return err
	}
	sockPath := SocketPath(rec.RunDir)
	shutdownErr := b.WithRunningVM(ctx, &rec, func(pid int) error {
		return spec.Shutdown(ctx, &rec, sockPath, pid)
	})
	settled := errors.Is(shutdownErr, ErrNotRunning) && rec.State == types.VMStateStopped && !hasOpenComputeInterval(&rec)
	if err := b.HandleStopResult(ctx, id, rec.RunDir, spec.RuntimeFiles, shutdownErr); err != nil {
		return err
	}
	transitioned := false
	if !settled {
		// Warn-and-continue: the VMM is dead and the flip self-heals on the next reconcile.
		logger := log.WithFunc(b.Typ + ".StopOneLocked")
		if err := b.UpdateStates(ctx, []string{id}, types.VMStateStopped); err != nil {
			logger.Warnf(ctx, "mark stopped %s: %v", id, err)
		} else {
			transitioned = true
		}
	}
	// Still under the caller's ops lock: an idle TAP's TC redirect storms softirqs until its host NICs go down (#130).
	b.quiesceAfterStop(ctx, id, &rec, transitioned)
	return nil
}

// StopAll mirrors StartAll: stopOne per ref, each flipping its own state under its VM's ops lock.
func (b *Backend) StopAll(ctx context.Context, refs []string, stopOne func(context.Context, string) error) ([]string, error) {
	ids, err := b.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	return b.ForEachVM(ctx, ids, "Stop", stopOne)
}

// DeleteAll removes VMs by ref; each VM's stop+probe+delete runs under its ops lock (#103), so stopLocked must not re-take it.
func (b *Backend) DeleteAll(ctx context.Context, refs []string, force bool, stopLocked func(context.Context, string) error) ([]string, error) {
	ids, err := b.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	// One /proc scan up-front; per-VM orphan check filters this cache instead of re-walking /proc N times.
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

func (b *Backend) HandleStopResult(ctx context.Context, id, runDir string, runtimeFiles []string, shutdownErr error) error {
	if shutdownErr != nil && !errors.Is(shutdownErr, ErrNotRunning) {
		b.MarkError(ctx, id)
		return shutdownErr
	}
	CleanupRuntimeFiles(ctx, runDir, runtimeFiles)
	return nil
}

// deleteOneLocked is DeleteAll's per-VM body, run under the ops lock.
func (b *Backend) deleteOneLocked(ctx context.Context, id string, force bool, stopLocked func(context.Context, string) error, rec *VMRecord, procScan utils.ProcScan) error {
	sockPath := SocketPath(rec.RunDir)
	stoppedByUs := false
	if runningErr := b.WithRunningVM(ctx, rec, func(_ int) error {
		if !force {
			return fmt.Errorf("running (force required)")
		}
		stoppedByUs = true
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
		if !utils.IsProcessAlive(pid) {
			continue
		}
		if termErr := utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, b.Conf.TerminateGracePeriod()); termErr != nil {
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
	computeReason := metering.ReasonStopCrash
	if stoppedByUs {
		computeReason = metering.ReasonStopUser
	}
	b.emitDeleteClose(ctx, id, shape, computeReason, hadRunningInterval)
	return nil
}
