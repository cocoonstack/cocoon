package hypervisor

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	StaleCreateCollected   StaleCreateOutcome = "collected"    // ownerless placeholder reclaimed; record and name freed
	StaleCreateBusy        StaleCreateOutcome = "busy"         // in-flight operation owns the VM; nothing touched
	StaleCreateNotCreating StaleCreateOutcome = "not-creating" // record left the creating state under the lock
	StaleCreateNotFound    StaleCreateOutcome = "not-found"    // no record under the id
)

// StaleCreateOutcome reports what ReconcileStaleCreate did with the record.
type StaleCreateOutcome string

// Supervisable is the backend surface a resident supervisor drives.
type Supervisable interface {
	Type() string
	ScanSupervision(ctx context.Context) (SupervisionScan, error)
	ObserveVMM(ctx context.Context, rec *VMRecord) (utils.ProcRef, error)
	ObserveVMMIn(ctx context.Context, rec *VMRecord, scan utils.ProcScan) (utils.ProcRef, error)
	TryLockVMOps(ctx context.Context, vmID string) (func(), bool, error)
	PeekRecord(ctx context.Context, vmID string) (*VMRecord, error)
	ConvergeDead(ctx context.Context, vmID string, gen uint64, observedAt time.Time) error
	ReconcileToRunning(ctx context.Context, vmID string) (uint64, error)
	ReconcileStaleCreate(ctx context.Context, vmID string) (StaleCreateOutcome, error)
	RecoverTombstone(ctx context.Context, vmID string) (bool, error)
}

// SupervisionScan is one reconcile pass's view of a backend namespace plus the single /proc walk its liveness checks share.
type SupervisionScan struct {
	Records    []*VMRecord
	Tombstoned map[string]struct{}
	Procs      utils.ProcScan
}

// ScanSupervision reads every record plus the unfinished-delete set in one transaction; the json engine holds the namespace flock for the whole closure, so never widen this into per-VM reads.
func (b *Backend) ScanSupervision(ctx context.Context) (SupervisionScan, error) {
	var scan SupervisionScan
	if err := b.view(ctx, func(t *vmTx) error {
		all, err := t.All()
		if err != nil {
			return err
		}
		ids, err := b.tombstones().PendingIDs(ctx, t.r)
		if err != nil {
			return err
		}
		scan.Records = slices.Collect(maps.Values(all))
		scan.Tombstoned = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			scan.Tombstoned[id] = struct{}{}
		}
		return nil
	}); err != nil {
		return SupervisionScan{}, err
	}
	procs, err := utils.ScanProcsByBinary(b.Conf.BinaryName())
	if err != nil {
		return SupervisionScan{}, fmt.Errorf("scan /proc for %s: %w", b.Typ, err)
	}
	scan.Procs = procs
	return scan, nil
}

// ObserveVMM returns the live VMM's process generation, ErrNotRunning when none is live, or an error when liveness is inconclusive.
func (b *Backend) ObserveVMM(ctx context.Context, rec *VMRecord) (utils.ProcRef, error) {
	return b.observe(ctx, rec, nil)
}

// ObserveVMMIn is ObserveVMM against a pass-wide /proc walk; mutations must still re-observe freshly under the ops lock.
func (b *Backend) ObserveVMMIn(ctx context.Context, rec *VMRecord, scan utils.ProcScan) (utils.ProcRef, error) {
	return b.observe(ctx, rec, &scan)
}

// TryLockVMOps takes the VM ops lock without blocking; ok=false with a nil error means another operation owns it.
func (b *Backend) TryLockVMOps(ctx context.Context, vmID string) (unlock func(), ok bool, err error) {
	l, err := opsLock(b.Conf, vmID)
	if err != nil {
		return nil, false, err
	}
	locked, err := l.TryLock(ctx)
	if err != nil || !locked {
		return nil, false, err
	}
	return func() { _ = l.Unlock(ctx) }, true, nil
}

// ConvergeDead lands the stop transition for a VMM that exited outside a cocoon stop, then runs the quiesce it schedules; the caller holds the ops lock and observed no live VMM. gen fences the write so a stale observation cannot date or re-label a newer transition.
func (b *Backend) ConvergeDead(ctx context.Context, id string, gen uint64, observedAt time.Time) error {
	if err := b.convergeDeadRecord(ctx, id, gen, observedAt); err != nil {
		return err
	}
	b.removeCgroupScope(ctx, id)
	return b.QuiesceIfPending(ctx, id)
}

// QuiesceIfPending runs a scheduled quiesce and clears the flag fenced on the generation it read; the caller holds the ops lock with no VMM live.
func (b *Backend) QuiesceIfPending(ctx context.Context, id string) error {
	if b.Net == nil {
		return nil
	}
	var (
		vm  *types.VM
		gen uint64
	)
	if err := b.view(ctx, func(t *vmTx) error {
		r, err := t.Get(id)
		if err != nil || r == nil || !r.QuiescePending {
			return err
		}
		vm, gen = &r.VM, r.TransitionGeneration
		return nil
	}); err != nil || vm == nil {
		return err
	}
	if err := b.quiesceNetwork(ctx, vm); err != nil {
		return fmt.Errorf("quiesce network for VM %s (pending kept): %w", id, err)
	}
	return b.clearQuiescePending(ctx, id, gen)
}

// ReconcileStaleCreate reclaims id when it is an ownerless creating placeholder; a free ops lock is the proof of ownerlessness, since create and clone hold it from prereserve through the final record commit.
func (b *Backend) ReconcileStaleCreate(ctx context.Context, id string) (StaleCreateOutcome, error) {
	unlock, ok, err := b.TryLockVMOps(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return StaleCreateBusy, nil
	}
	defer unlock()
	rec, err := b.PeekRecord(ctx, id)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return StaleCreateNotFound, nil
	}
	if rec.State != types.VMStateCreating {
		return StaleCreateNotCreating, nil
	}
	if err := b.collectStaleCreate(ctx, id, rec); err != nil {
		return "", err
	}
	return StaleCreateCollected, nil
}

// RecoverTombstone drives an unfinished delete to completion under the held ops lock; supervision starts deletes of its own, so it must be able to finish them.
func (b *Backend) RecoverTombstone(ctx context.Context, id string) (bool, error) {
	return b.recoverVMTombstone(ctx, id)
}

func (b *Backend) observe(ctx context.Context, rec *VMRecord, scan *utils.ProcScan) (utils.ProcRef, error) {
	var ref utils.ProcRef
	err := b.withRunningVM(ctx, rec, scan, func(pid int) error {
		var refErr error
		ref, refErr = utils.ProcRefOf(pid)
		return refErr
	})
	return ref, err
}

// convergeDeadRecord commits the stop transition and publishes its staged metering entry; the quiesce it schedules is the caller's to run.
func (b *Backend) convergeDeadRecord(ctx context.Context, id string, gen uint64, observedAt time.Time) error {
	var emit []metering.Entry
	if err := b.update(ctx, func(t *vmTx) error {
		emit = nil
		r, err := t.Get(id)
		if err != nil || r == nil {
			return err
		}
		if r.TransitionGeneration != gen || !NeedsDeadConvergence(r) {
			return nil
		}
		if hasOpenComputeInterval(r) {
			emit = []metering.Entry{b.makeEntry(metering.KindVMComputeStop, id, metering.ReasonStopCrash, shapeFromConfig(r.Config), observedAt)}
		}
		// StoppedAt is observation time, owed even to records predating the interval bookkeeping.
		r.StoppedAt = &observedAt
		markTransition(r, types.VMStateStopped, types.TransitionUnexpectedExit, observedAt)
		r.QuiescePending = needsQuiesce(r)
		return t.Put(id, r)
	}); err != nil {
		return err
	}
	b.emitAll(ctx, emit)
	return nil
}

// collectStaleCreate runs the reclaim under the caller's held ops lock: no orphan VMM may survive, then the tombstoned delete protocol.
func (b *Backend) collectStaleCreate(ctx context.Context, id string, rec *VMRecord) error {
	if err := b.ensureOrphanVMMDead(ctx, rec.RunDir); err != nil {
		return fmt.Errorf("orphan vmm for %s: %w (dirs kept)", id, err)
	}
	return b.deleteVMProtocol(ctx, id, rec)
}

// clearQuiescePending is relaxed: losing the clear only costs one idempotent re-quiesce on a later pass.
func (b *Backend) clearQuiescePending(ctx context.Context, id string, gen uint64) error {
	return b.updateRelaxed(ctx, func(t *vmTx) error {
		r, err := t.Get(id)
		if err != nil || r == nil || r.TransitionGeneration != gen || !r.QuiescePending {
			return err
		}
		r.QuiescePending = false
		return t.Put(id, r, meta.RelaxedOK)
	})
}

// convergeCrashedStart closes a crashed VM's ledger on the start path, skipping the quiesce the imminent launch would undo; the in-memory precheck exists because even an empty transaction takes the namespace flock.
func (b *Backend) convergeCrashedStart(ctx context.Context, rec *VMRecord) {
	if !NeedsDeadConvergence(rec) {
		return
	}
	if err := b.convergeDeadRecord(ctx, rec.ID, rec.TransitionGeneration, timeNow()); err != nil {
		log.WithFunc(b.Typ+".convergeCrashedStart").Warnf(ctx, "converge crashed VM %s: %v", rec.ID, err)
	}
}

// NeedsDeadConvergence reports whether a record still claims a VM as up; creating placeholders are the stale-create protocol's to collect.
func NeedsDeadConvergence(r *VMRecord) bool {
	if r == nil || r.State == types.VMStateCreating {
		return false
	}
	return r.State == types.VMStateRunning || hasOpenComputeInterval(r)
}
