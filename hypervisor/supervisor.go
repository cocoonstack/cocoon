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

var _ Supervisable = (*Backend)(nil)

// Supervisable is the backend surface a resident supervisor drives; every
// hypervisor backend embeds *Backend and satisfies it.
type Supervisable interface {
	Type() string
	ScanSupervision(ctx context.Context) (SupervisionScan, error)
	ObserveVMM(ctx context.Context, rec *VMRecord) (utils.ProcRef, error)
	ObserveVMMIn(ctx context.Context, rec *VMRecord, scan utils.ProcScan) (utils.ProcRef, error)
	TryLockVMOps(ctx context.Context, vmID string) (func(), bool, error)
	PeekRecord(ctx context.Context, vmID string) (*VMRecord, error)
	ConvergeDead(ctx context.Context, vmID string, gen uint64, observedAt time.Time) error
	ReconcileToRunning(ctx context.Context, vmID string) uint64
	CollectStaleCreate(ctx context.Context, vmID string, rec *VMRecord) error
}

// SupervisionScan is one reconcile pass's view of a backend namespace, plus the
// single /proc walk its liveness checks share.
type SupervisionScan struct {
	Records    []*VMRecord
	Tombstoned map[string]struct{}
	Procs      utils.ProcScan
}

// ScanSupervision reads every record plus the unfinished-delete set in one
// transaction: the json engine holds the namespace lock for the whole closure,
// so a pass must never widen this into per-VM reads.
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
	// Outside the transaction: the json engine holds the namespace flock for the
	// whole closure, and this walks every process on the host.
	procs, err := utils.ScanProcsByBinary(b.Conf.BinaryName())
	if err != nil {
		return SupervisionScan{}, fmt.Errorf("scan /proc for %s: %w", b.Typ, err)
	}
	scan.Procs = procs
	return scan, nil
}

// ObserveVMM returns the live VMM's process generation, ErrNotRunning when none
// is live, or an error when liveness is inconclusive and the caller must retry.
func (b *Backend) ObserveVMM(ctx context.Context, rec *VMRecord) (utils.ProcRef, error) {
	return b.observe(ctx, rec, nil)
}

// ObserveVMMIn is ObserveVMM against a pass-wide /proc walk. It is for deciding
// what work to attempt: anything that then mutates must re-observe freshly under
// the ops lock, because the scan predates the lock.
func (b *Backend) ObserveVMMIn(ctx context.Context, rec *VMRecord, scan utils.ProcScan) (utils.ProcRef, error) {
	return b.observe(ctx, rec, &scan)
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

// TryLockVMOps takes the VM ops lock without blocking; ok=false with a nil error
// means another operation owns it and the caller should retry on a later pass.
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

// ConvergeDead lands the transition for a VMM that exited outside a cocoon stop,
// then runs any network quiesce it schedules. The caller holds the VM ops lock
// and has conclusively observed that no VMM generation is live. gen fences the
// write against a record that transitioned since the observation, so a stale
// event cannot date or re-label a newer transition; observedAt dates the stop.
func (b *Backend) ConvergeDead(ctx context.Context, id string, gen uint64, observedAt time.Time) error {
	if err := b.convergeDeadRecord(ctx, id, gen, observedAt); err != nil {
		return err
	}
	return b.QuiesceIfPending(ctx, id)
}

// convergeDeadRecord commits the stop transition and publishes its staged
// metering entry; the network quiesce it schedules is the caller's to run.
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
			r.StoppedAt = &observedAt
		}
		markTransition(r, types.VMStateStopped, types.TransitionUnexpectedExit, observedAt)
		r.QuiescePending = needsQuiesce(r)
		return t.Put(id, r)
	}); err != nil {
		return err
	}
	b.emitAll(ctx, emit)
	return nil
}

// QuiesceIfPending brings a stopped VM's host NICs down and clears the flag
// under the generation it observed, so a VM restarted in between keeps the
// pending work its own stop scheduled. The caller holds the VM ops lock and has
// established that no VMM is live.
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

// CollectStaleCreate terminates any orphan VMM and runs the delete protocol for
// a creating placeholder whose owner is gone, freeing the name reservation. The
// caller holds the VM ops lock, which is itself the proof of ownerlessness:
// create and clone hold it from prereserve through the final record commit.
func (b *Backend) CollectStaleCreate(ctx context.Context, id string, rec *VMRecord) error {
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

// convergeCrashedStart closes a crashed VM's ledger interval on the start path.
// It schedules no quiesce of its own: the launch about to follow brings the same
// plumbing up, and a failed launch leaves the pending flag for a later pass.
func (b *Backend) convergeCrashedStart(ctx context.Context, rec *VMRecord) {
	// Prechecked in memory: rec is fresh from the entry guard under the ops lock, and a
	// transaction that would decide there is nothing to do still takes the namespace flock.
	if !NeedsDeadConvergence(rec) {
		return
	}
	if err := b.convergeDeadRecord(ctx, rec.ID, rec.TransitionGeneration, timeNow()); err != nil {
		log.WithFunc(b.Typ+".convergeCrashedStart").Warnf(ctx, "converge crashed VM %s: %v", rec.ID, err)
	}
}

// NeedsDeadConvergence reports whether a record still claims a VM the ledger or the state field thinks is up. Creating placeholders are excluded: their owner is the create path, and the stale-create protocol collects them.
func NeedsDeadConvergence(r *VMRecord) bool {
	if r == nil || r.State == types.VMStateCreating {
		return false
	}
	return r.State == types.VMStateRunning || hasOpenComputeInterval(r)
}
