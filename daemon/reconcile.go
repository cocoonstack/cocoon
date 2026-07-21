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
	all := make([]VMStatus, 0, d.state.size())
	healthy := true
	for _, b := range d.order {
		st, err := d.reconcileBackend(ctx, b)
		if err != nil {
			healthy = false
			continue
		}
		all = append(all, st...)
	}
	d.state.publish(all, healthy, time.Now())
}

func (d *Daemon) reconcileBackend(ctx context.Context, b Supervisor) ([]VMStatus, error) {
	scan, err := b.ScanSupervision(ctx)
	if err != nil {
		log.WithFunc("daemon.reconcileBackend").Errorf(ctx, err, "scan %s", b.Type())
		return nil, err
	}
	now := time.Now()
	seen := make(map[string]struct{}, len(scan.Records))
	out := make([]VMStatus, 0, len(scan.Records))
	for _, rec := range scan.Records {
		seen[rec.ID] = struct{}{}
		key := watchKey{backend: b.Type(), vmID: rec.ID}
		live := false
		if _, tombstoned := scan.Tombstoned[rec.ID]; !tombstoned {
			live = d.reconcileVM(ctx, b, key, rec)
		}
		out = append(out, newVMStatus(b.Type(), rec, live, d.watcher.pidOf(key), now))
	}
	d.watcher.dropAbsent(b.Type(), seen)
	return out, nil
}

// reconcileVM applies the supervision rules to one record and reports whether a VMM generation is live.
func (d *Daemon) reconcileVM(ctx context.Context, b Supervisor, key watchKey, rec *hypervisor.VMRecord) bool {
	// A creating placeholder is never promoted: a clone launches its VMM before the record is complete.
	if rec.State == types.VMStateCreating {
		d.collectOwnerless(ctx, b, rec)
		return false
	}
	proc, err := b.ObserveVMM(ctx, rec)
	switch {
	case err == nil:
		d.adoptLive(ctx, b, key, rec, proc)
		return true
	case errors.Is(err, hypervisor.ErrNotRunning):
		d.convergeDead(ctx, b, key, rec)
		return false
	default:
		// Fail closed: an inconclusive probe must never be read as an exit.
		log.WithFunc("daemon.reconcileVM").Warnf(ctx, "liveness inconclusive for %s/%s: %v", key.backend, key.vmID, err)
		return false
	}
}

// adoptLive watches the exact live generation, repairing a record that drifted out of Running first.
func (d *Daemon) adoptLive(ctx context.Context, b Supervisor, key watchKey, rec *hypervisor.VMRecord, proc utils.ProcRef) {
	if rec.State == types.VMStateRunning {
		d.watcher.ensure(key, proc, rec.TransitionGeneration)
		return
	}
	if rec.Quarantine != "" {
		return
	}
	unlock, ok := d.tryLock(ctx, b, key.vmID)
	if !ok {
		return
	}
	defer unlock()
	fresh, err := b.PeekRecord(ctx, key.vmID)
	if err != nil || fresh == nil || fresh.Quarantine != "" {
		return
	}
	if fresh.State == types.VMStateRunning || fresh.State == types.VMStateCreating {
		return
	}
	if again, obsErr := b.ObserveVMM(ctx, fresh); obsErr != nil || again != proc {
		return
	}
	b.ReconcileToRunning(ctx, key.vmID)
	log.WithFunc("daemon.adoptLive").Warnf(ctx, "adopted live VM %s whose record read %s", key.vmID, fresh.State)
	// Re-read: the repair bumped the generation the watch must fence against.
	if after, err := b.PeekRecord(ctx, key.vmID); err == nil && after != nil {
		d.watcher.ensure(key, proc, after.TransitionGeneration)
	}
}

// convergeDead lands the stop transition, or retries a quiesce the last stop could not finish.
func (d *Daemon) convergeDead(ctx context.Context, b Supervisor, key watchKey, rec *hypervisor.VMRecord) {
	d.watcher.drop(key)
	if !hypervisor.NeedsDeadConvergence(rec) && !rec.QuiescePending {
		return
	}
	unlock, ok := d.tryLock(ctx, b, key.vmID)
	if !ok {
		return
	}
	defer unlock()
	fresh, err := b.PeekRecord(ctx, key.vmID)
	if err != nil || fresh == nil {
		return
	}
	if !d.confirmDead(ctx, b, fresh) {
		return
	}
	logger := log.WithFunc("daemon.convergeDead")
	if hypervisor.NeedsDeadConvergence(fresh) {
		if err := b.ConvergeDead(ctx, key.vmID, fresh.TransitionGeneration, time.Now()); err != nil {
			logger.Errorf(ctx, err, "converge %s", key.vmID)
			return
		}
		logger.Warnf(ctx, "VM %s exited outside a cocoon stop", key.vmID)
		return
	}
	if fresh.QuiescePending {
		if err := b.QuiesceIfPending(ctx, key.vmID); err != nil {
			logger.Errorf(ctx, err, "retry quiesce %s", key.vmID)
		}
	}
}

// collectOwnerless reclaims a creating placeholder whose owner died; a free ops lock is the proof, since create and clone hold it from prereserve through the final record commit.
func (d *Daemon) collectOwnerless(ctx context.Context, b Supervisor, rec *hypervisor.VMRecord) {
	unlock, ok := d.tryLock(ctx, b, rec.ID)
	if !ok {
		return
	}
	defer unlock()
	fresh, err := b.PeekRecord(ctx, rec.ID)
	if err != nil || fresh == nil || fresh.State != types.VMStateCreating {
		return
	}
	logger := log.WithFunc("daemon.collectOwnerless")
	if err := b.CollectStaleCreate(ctx, rec.ID, fresh); err != nil {
		logger.Errorf(ctx, err, "collect ownerless create %s", rec.ID)
		return
	}
	logger.Warnf(ctx, "collected ownerless creating VM %s", rec.ID)
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
	if b == nil {
		return false
	}
	unlock, ok := d.tryLock(ctx, b, ev.key.vmID)
	if !ok {
		return false
	}
	defer unlock()
	rec, err := b.PeekRecord(ctx, ev.key.vmID)
	if err != nil || rec == nil {
		return false
	}
	// A newer transition owns the record; this event may not date or label it.
	if rec.TransitionGeneration != ev.gen || !d.confirmDead(ctx, b, rec) {
		return false
	}
	logger := log.WithFunc("daemon.handleExit")
	if err := b.ConvergeDead(ctx, ev.key.vmID, ev.gen, ev.at); err != nil {
		logger.Errorf(ctx, err, "converge %s", ev.key.vmID)
		return false
	}
	logger.Warnf(ctx, "VM %s exited outside a cocoon stop", ev.key.vmID)
	return true
}

// confirmDead re-observes under the ops lock; a live or inconclusive result defers the work to a later pass.
func (d *Daemon) confirmDead(ctx context.Context, b Supervisor, rec *hypervisor.VMRecord) bool {
	_, err := b.ObserveVMM(ctx, rec)
	return errors.Is(err, hypervisor.ErrNotRunning)
}

// tryLock reports ok=false for a busy lock and for a lock error alike; both retry next pass, only the error is logged.
func (d *Daemon) tryLock(ctx context.Context, b Supervisor, vmID string) (func(), bool) {
	unlock, ok, err := b.TryLockVMOps(ctx, vmID)
	if err != nil {
		log.WithFunc("daemon.tryLock").Errorf(ctx, err, "ops lock for %s", vmID)
		return nil, false
	}
	return unlock, ok
}
