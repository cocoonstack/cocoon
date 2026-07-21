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

// NetLifecycle converges a VM's host networking. The command layer supplies it
// so a state transition and its network action share one VM ops lock instead of
// splitting across an unlock window.
type NetLifecycle struct {
	Recover func(ctx context.Context, vm *types.VM) error
	Quiesce func(ctx context.Context, vm *types.VM) error
}

// SupervisionScan is one reconcile pass's view of a backend namespace.
type SupervisionScan struct {
	Records    []*VMRecord
	Tombstoned map[string]struct{}
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
	return scan, nil
}

// ObserveVMM returns the live VMM's process generation, ErrNotRunning when none
// is live, or an error when liveness is inconclusive and the caller must retry.
func (b *Backend) ObserveVMM(ctx context.Context, rec *VMRecord) (utils.ProcRef, error) {
	var ref utils.ProcRef
	err := b.WithRunningVM(ctx, rec, func(pid int) error {
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
	pending, err := b.convergeDeadRecord(ctx, id, gen, observedAt)
	if err != nil || !pending {
		return err
	}
	return b.QuiesceIfPending(ctx, id)
}

// convergeDeadRecord commits the stop transition and publishes its staged
// metering entry; the network quiesce it schedules is the caller's to run.
func (b *Backend) convergeDeadRecord(ctx context.Context, id string, gen uint64, observedAt time.Time) (bool, error) {
	var (
		emit    []metering.Entry
		pending bool
	)
	if err := b.update(ctx, func(t *vmTx) error {
		emit, pending = nil, false
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
		pending = r.QuiescePending
		return t.Put(id, r)
	}); err != nil {
		return false, err
	}
	b.emitAll(ctx, emit)
	return pending, nil
}

// QuiesceIfPending brings a stopped VM's host NICs down and clears the flag
// under the generation it observed, so a VM restarted in between keeps the
// pending work its own stop scheduled. The caller holds the VM ops lock and has
// established that no VMM is live.
func (b *Backend) QuiesceIfPending(ctx context.Context, id string) error {
	if b.NetLife.Quiesce == nil {
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
	if err := b.NetLife.Quiesce(ctx, vm); err != nil {
		return fmt.Errorf("quiesce network for VM %s (pending kept): %w", id, err)
	}
	return b.clearQuiescePending(ctx, id, gen)
}

// RecoverNetwork rebuilds and un-quiesces a VM's host plumbing before launch;
// the caller holds the VM ops lock.
func (b *Backend) RecoverNetwork(ctx context.Context, rec *VMRecord) error {
	if b.NetLife.Recover == nil {
		return nil
	}
	return b.NetLife.Recover(ctx, &rec.VM)
}

// CollectStaleCreate terminates any orphan VMM and runs the delete protocol for
// a creating placeholder whose owner is gone, freeing the name reservation. The
// caller holds the VM ops lock, which is itself the proof of ownerlessness:
// create and clone hold it from prereserve through the final record commit.
func (b *Backend) CollectStaleCreate(ctx context.Context, id string, rec *VMRecord) error {
	if err := b.ensureOrphanVMMDead(ctx, rec.RunDir); err != nil {
		return fmt.Errorf("orphan vmm for %s: %w (dirs kept)", id, err)
	}
	return b.deleteVMProtocol(ctx, id, rec, b.NetCleanup)
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
	if _, err := b.convergeDeadRecord(ctx, rec.ID, rec.TransitionGeneration, timeNow()); err != nil {
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
