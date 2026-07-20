package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	if err := b.HandleStopResult(ctx, id, rec.RunDir, spec.RuntimeFiles, shutdownErr); err != nil {
		return err
	}
	// Warn-and-continue: the VMM is dead and the flip self-heals on the next reconcile.
	if err := b.UpdateStates(ctx, []string{id}, types.VMStateStopped); err != nil {
		log.WithFunc(b.Typ+".StopOneLocked").Warnf(ctx, "mark stopped %s: %v", id, err)
	}
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

// DeleteAll removes VMs by ref; each VM's stop+probe+delete runs under its ops lock (#103), so stopLocked must not re-take it. wrap (optional) encloses the locked per-VM deletion — FC passes its writable-disk locks so a clone's symlink-redirect window cannot be deleted from under it.
func (b *Backend) DeleteAll(ctx context.Context, refs []string, force bool, stopLocked func(context.Context, string) error, wrap func(rec *VMRecord, fn func() error) error) ([]string, error) {
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
		return runWrapped(&rec, wrap, func() error {
			return b.deleteOneLocked(ctx, id, force, stopLocked, &rec, procScan)
		})
	})
}

// deleteOneLocked is DeleteAll's per-VM body, run under the ops lock (and the backend wrap).
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
	var (
		shape              metering.Shape
		hadRunningInterval bool
	)
	// Dirs outside the configured roots escape the GC orphan scan; persist a cleanup intent in the same transaction so a failed removal stays reclaimable.
	migrated := migratedDirs(rec, b.Conf.VMRunDir(id), b.Conf.VMLogDir(id))
	// Record first: dir removal deletes the ops.lock inode, and a fresh-inode locker must resolve a gone record instead of reviving the VM.
	if err := b.update(ctx, func(t *vmTx) error {
		r, err := t.Get(id)
		if err != nil {
			return err
		}
		if r == nil {
			return ErrNotFound
		}
		hadRunningInterval = hasOpenComputeInterval(r)
		shape = shapeFromConfig(r.Config)
		if err := t.NameDel(r.Config.Name); err != nil {
			return err
		}
		if err := t.Del(id); err != nil {
			return err
		}
		for _, dir := range migrated {
			if err := t.addOrphanDir(dir); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	computeReason := metering.ReasonStopCrash
	if stoppedByUs {
		computeReason = metering.ReasonStopUser
	}
	b.emitDeleteClose(ctx, id, shape, computeReason, hadRunningInterval)
	if rmErr := RemoveVMDirs(rec.RunDir, rec.LogDir); rmErr != nil {
		return fmt.Errorf("cleanup VM dirs (gc will retry): %w", rmErr)
	}
	if len(migrated) > 0 {
		b.clearOrphanDirs(ctx, migrated)
	}
	return nil
}

func (b *Backend) clearOrphanDirs(ctx context.Context, dirs []string) {
	if err := b.update(ctx, func(t *vmTx) error {
		for _, dir := range dirs {
			if err := t.removeOrphanDir(dir); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		log.WithFunc(b.Typ+".clearOrphanDirs").Warnf(ctx, "clear cleanup intents %v: %v", dirs, err)
	}
}

func (b *Backend) HandleStopResult(ctx context.Context, id, runDir string, runtimeFiles []string, shutdownErr error) error {
	if shutdownErr != nil && !errors.Is(shutdownErr, ErrNotRunning) {
		b.MarkError(ctx, id)
		return shutdownErr
	}
	CleanupRuntimeFiles(ctx, runDir, runtimeFiles)
	return nil
}

// migratedDirs returns rec dirs that differ from the configured defaults ("" entries dropped).
func migratedDirs(rec *VMRecord, defRunDir, defLogDir string) []string {
	var dirs []string
	if rec.RunDir != "" && filepath.Clean(rec.RunDir) != filepath.Clean(defRunDir) {
		dirs = append(dirs, filepath.Clean(rec.RunDir))
	}
	if rec.LogDir != "" && filepath.Clean(rec.LogDir) != filepath.Clean(defLogDir) {
		dirs = append(dirs, filepath.Clean(rec.LogDir))
	}
	return dirs
}
