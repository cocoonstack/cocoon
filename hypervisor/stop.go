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
	// Distinguish user cancellation from timeout.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Warnf(ctx, "VM %s did not shut down within %s, escalating", vmID, timeout)
	return escalate()
}

// StopOneSequence runs the shared per-id stop skeleton (LoadRecord → WithRunningVM(Shutdown) → HandleStopResult) so backends only express their force-vs-graceful choice.
func (b *Backend) StopOneSequence(ctx context.Context, id string, spec StopSpec) error {
	rec, err := b.LoadRecord(ctx, id)
	if err != nil {
		return err
	}
	sockPath := SocketPath(rec.RunDir)
	shutdownErr := b.WithRunningVM(ctx, &rec, func(pid int) error {
		return spec.Shutdown(ctx, &rec, sockPath, pid)
	})
	return b.HandleStopResult(ctx, id, rec.RunDir, spec.RuntimeFiles, shutdownErr)
}

// StopAll mirrors StartAll: stopOne per ref, batch-flip succeeded to Stopped.
func (b *Backend) StopAll(ctx context.Context, refs []string, stopOne func(context.Context, string) error) ([]string, error) {
	ids, err := b.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	succeeded, forEachErr := b.ForEachVM(ctx, ids, "Stop", stopOne)
	if batchErr := b.UpdateStates(ctx, succeeded, types.VMStateStopped); batchErr != nil {
		log.WithFunc(b.Typ+".Stop").Warnf(ctx, "batch state update: %v", batchErr)
	}
	return succeeded, forEachErr
}

// DeleteAll removes VMs by ref; dir cleanup before DB delete keeps a failed cleanup retry-able.
func (b *Backend) DeleteAll(ctx context.Context, refs []string, force bool, stopOne func(context.Context, string) error) ([]string, error) {
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
		rec, loadErr := b.LoadRecord(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		sockPath := SocketPath(rec.RunDir)
		stoppedByUs := false
		if runningErr := b.WithRunningVM(ctx, &rec, func(_ int) error {
			if !force {
				return fmt.Errorf("running (force required)")
			}
			stoppedByUs = true
			return stopOne(ctx, id)
		}); runningErr != nil && !errors.Is(runningErr, ErrNotRunning) {
			return fmt.Errorf("stop before delete: %w", runningErr)
		}
		// Probe fires unconditionally: AF_UNIX has no TIME_WAIT, and catches false-negative pidfile/cmdline shortcuts.
		if live, probeErr := b.IsAPISocketLive(ctx, &rec); live {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if probeErr != nil {
				return fmt.Errorf("refuse delete: api socket %s probe inconclusive: %w (resolve the host issue or kill the vmm process then retry)", sockPath, probeErr)
			}
			return fmt.Errorf("refuse delete: api socket %s still responsive (suspected orphan vmm; kill the vmm process then retry)", sockPath)
		}
		for _, pid := range procScan.Find(sockPath) {
			// procScan is a pre-stopOne snapshot: a just-force-stopped VM is already gone.
			// Only a still-live match is a real orphan worth killing and logging.
			if !utils.IsProcessAlive(pid) {
				continue
			}
			if termErr := utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, b.Conf.TerminateGracePeriod()); termErr != nil {
				return fmt.Errorf("terminate orphan VMM pid=%d for VM %s: %w", pid, id, termErr)
			}
			log.WithFunc(b.Typ+".Delete").Warnf(ctx, "killed orphan VMM pid=%d for VM %s", pid, id)
		}
		if rmErr := RemoveVMDirs(rec.RunDir, rec.LogDir); rmErr != nil {
			return fmt.Errorf("cleanup VM dirs: %w", rmErr)
		}
		var (
			shape              metering.Shape
			hadRunningInterval bool
		)
		// Capture in the same transaction as delete so a concurrent UpdateStates can't shift the truth.
		if err := b.DB.Update(ctx, func(idx *VMIndex) error {
			r := idx.VMs[id]
			if r == nil {
				return ErrNotFound
			}
			hadRunningInterval = hasOpenComputeInterval(r)
			shape = shapeFromConfig(r.Config)
			delete(idx.Names, r.Config.Name)
			delete(idx.VMs, id)
			return nil
		}); err != nil {
			return err
		}
		computeReason := metering.ReasonStopCrash
		if stoppedByUs {
			computeReason = metering.ReasonStopUser
		}
		b.emitDeleteClose(ctx, id, shape, computeReason, hadRunningInterval)
		return nil
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
