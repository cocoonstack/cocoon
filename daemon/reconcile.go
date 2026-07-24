package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// reconcile runs one idempotent pass over every backend and republishes the API snapshot.
func (d *Daemon) reconcile(ctx context.Context) {
	var all []VMStatus
	healthy, degraded := true, 0
	for _, b := range d.order {
		st, failed, err := d.reconcileBackend(ctx, b)
		// Only a scan failure is unhealthy: a restart fixes an unreadable store, not a VM nobody can converge.
		if err != nil {
			healthy = false
		}
		degraded += failed
		all = append(all, st...)
	}
	d.state.publish(all, healthy, degraded, time.Now())
}

// reconcileBackend returns the backend's statuses, how many VMs the pass could not converge, and any scan failure; a busy ops lock is not a failure.
func (d *Daemon) reconcileBackend(ctx context.Context, b Supervisor) ([]VMStatus, int, error) {
	scan, err := b.ScanSupervision(ctx)
	if err != nil {
		log.WithFunc("daemon.reconcileBackend").Errorf(ctx, err, "scan %s", b.Type())
		return nil, 0, err
	}
	now := time.Now()
	seen := make(map[string]struct{}, len(scan.Records))
	out := make([]VMStatus, 0, len(scan.Records))
	failed := 0
	for _, rec := range scan.Records {
		seen[rec.ID] = struct{}{}
		key := watchKey{backend: b.Type(), vmID: rec.ID}
		live := false
		var vmErr error
		if _, tombstoned := scan.Tombstoned[rec.ID]; tombstoned {
			vmErr = d.resumeDelete(ctx, b, rec.ID)
		} else {
			live, vmErr = d.reconcileVM(ctx, b, key, rec, scan.Procs)
		}
		if vmErr != nil {
			failed++
		}
		out = append(out, newVMStatus(b.Type(), rec, live, d.watcher.pidOf(key), now))
	}
	d.watcher.dropAbsent(b.Type(), seen)
	return out, failed, nil
}

// reconcileVM applies the supervision rules to one record and reports whether a VMM generation is live.
func (d *Daemon) reconcileVM(ctx context.Context, b Supervisor, key watchKey, rec *hypervisor.VMRecord, procs utils.ProcScan) (bool, error) {
	// A creating placeholder is never promoted: a clone launches its VMM before the record is complete.
	if rec.State == types.VMStateCreating {
		return false, d.collectOwnerless(ctx, b, rec)
	}
	proc, err := b.ObserveVMMIn(ctx, rec, procs)
	switch {
	case err == nil:
		return true, d.adoptLive(ctx, b, key, rec, proc)
	case errors.Is(err, hypervisor.ErrNotRunning):
		return false, d.convergeDead(ctx, b, key, rec)
	default:
		// Fail closed: an inconclusive probe must never be read as an exit.
		log.WithFunc("daemon.reconcileVM").Warnf(ctx, "liveness inconclusive for %s/%s: %v", key.backend, key.vmID, err)
		return false, err
	}
}

// adoptLive watches the exact live generation, repairing a record that drifted out of Running first.
func (d *Daemon) adoptLive(ctx context.Context, b Supervisor, key watchKey, rec *hypervisor.VMRecord, proc utils.ProcRef) error {
	if rec.State == types.VMStateRunning {
		d.watcher.ensure(key, proc, rec.TransitionGeneration)
		return nil
	}
	if rec.Quarantine != "" {
		return nil
	}
	unlock, ok, err := d.tryLock(ctx, b, key.vmID)
	if !ok {
		return err
	}
	defer unlock()
	fresh, err := b.PeekRecord(ctx, key.vmID)
	if err != nil || fresh == nil || fresh.Quarantine != "" {
		return err
	}
	if again, obsErr := b.ObserveVMM(ctx, fresh); obsErr != nil || again != proc {
		return obsErr
	}
	gen, err := b.ReconcileToRunning(ctx, key.vmID)
	if err != nil || gen == 0 {
		return err
	}
	log.WithFunc("daemon.adoptLive").Warnf(ctx, "adopted live VM %s whose record read %s", key.vmID, fresh.State)
	d.watcher.ensure(key, proc, gen)
	return nil
}

// convergeDead lands the stop transition, or retries a quiesce the last stop could not finish.
func (d *Daemon) convergeDead(ctx context.Context, b Supervisor, key watchKey, rec *hypervisor.VMRecord) error {
	d.watcher.drop(key)
	if !hypervisor.NeedsDeadConvergence(rec) && !rec.QuiescePending {
		return nil
	}
	unlock, ok, err := d.tryLock(ctx, b, key.vmID)
	if !ok {
		return err
	}
	defer unlock()
	fresh, err := b.PeekRecord(ctx, key.vmID)
	if err != nil || fresh == nil {
		return err
	}
	dead, err := d.confirmDead(ctx, b, fresh)
	if !dead {
		return err
	}
	logger := log.WithFunc("daemon.convergeDead")
	exited := hypervisor.NeedsDeadConvergence(fresh)
	if err := b.ConvergeDead(ctx, key.vmID, fresh.TransitionGeneration, time.Now()); err != nil {
		logger.Errorf(ctx, err, "converge %s", key.vmID)
		return err
	}
	if exited {
		logger.Warnf(ctx, "VM %s exited outside a cocoon stop", key.vmID)
	}
	return nil
}

// collectOwnerless reclaims a creating placeholder whose owner died; a free ops lock is the proof, since create and clone hold it from prereserve through the final record commit.
func (d *Daemon) collectOwnerless(ctx context.Context, b Supervisor, rec *hypervisor.VMRecord) error {
	unlock, ok, err := d.tryLock(ctx, b, rec.ID)
	if !ok {
		return err
	}
	defer unlock()
	fresh, err := b.PeekRecord(ctx, rec.ID)
	if err != nil || fresh == nil || fresh.State != types.VMStateCreating {
		return err
	}
	logger := log.WithFunc("daemon.collectOwnerless")
	if err := b.CollectStaleCreate(ctx, rec.ID, fresh); err != nil {
		logger.Errorf(ctx, err, "collect ownerless create %s", rec.ID)
		return err
	}
	logger.Warnf(ctx, "collected ownerless creating VM %s", rec.ID)
	return nil
}

// resumeDelete finishes a delete a previous worker left mid-protocol; supervision starts deletes of its own, and an unfinished one strands the record and its name until a manual gc.
func (d *Daemon) resumeDelete(ctx context.Context, b Supervisor, vmID string) error {
	unlock, ok, err := d.tryLock(ctx, b, vmID)
	if !ok {
		return err
	}
	defer unlock()
	logger := log.WithFunc("daemon.resumeDelete")
	done, err := b.RecoverTombstone(ctx, vmID)
	if err != nil {
		logger.Errorf(ctx, err, "resume interrupted delete of %s", vmID)
		return err
	}
	if done {
		logger.Warnf(ctx, "finished an interrupted delete of VM %s", vmID)
	}
	return nil
}

// handleExit converges the generation the watcher saw exit, dating the stop from that observation, then republishes for stream consumers.
func (d *Daemon) handleExit(ctx context.Context, ev exitEvent) {
	converged := d.convergeExit(ctx, ev)
	for {
		select {
		case next := <-d.watcher.exits:
			// Coalesce a burst: a host-wide kill must cost one pass, not one per VM.
			converged = d.convergeExit(ctx, next) || converged
		default:
			if converged {
				d.reconcile(ctx)
			}
			return
		}
	}
}

// convergeExit runs the locked half of handleExit; the pass that follows must not be inside the lock it takes.
func (d *Daemon) convergeExit(ctx context.Context, ev exitEvent) bool {
	b := d.backends[ev.key.backend]
	unlock, ok, _ := d.tryLock(ctx, b, ev.key.vmID)
	if !ok {
		return false
	}
	defer unlock()
	rec, err := b.PeekRecord(ctx, ev.key.vmID)
	if err != nil || rec == nil {
		return false
	}
	// A newer transition owns the record; this event may not date or label it.
	if dead, _ := d.confirmDead(ctx, b, rec); rec.TransitionGeneration != ev.gen || !dead {
		return false
	}
	logger := log.WithFunc("daemon.convergeExit")
	if err := b.ConvergeDead(ctx, ev.key.vmID, ev.gen, ev.at); err != nil {
		logger.Errorf(ctx, err, "converge %s", ev.key.vmID)
		return false
	}
	logger.Warnf(ctx, "VM %s exited outside a cocoon stop", ev.key.vmID)
	return true
}

// confirmDead re-observes under the ops lock; live and inconclusive both defer the work, but only inconclusive is a failure worth counting.
func (d *Daemon) confirmDead(ctx context.Context, b Supervisor, rec *hypervisor.VMRecord) (bool, error) {
	_, err := b.ObserveVMM(ctx, rec)
	if errors.Is(err, hypervisor.ErrNotRunning) {
		return true, nil
	}
	return false, err
}

// tryLock reports ok=false for a busy lock and for a lock error alike; only the error is worth logging, and only it makes the pass unhealthy.
func (d *Daemon) tryLock(ctx context.Context, b Supervisor, vmID string) (func(), bool, error) {
	unlock, ok, err := b.TryLockVMOps(ctx, vmID)
	if err != nil {
		log.WithFunc("daemon.tryLock").Errorf(ctx, err, "ops lock for %s", vmID)
		return nil, false, err
	}
	return unlock, ok, nil
}
