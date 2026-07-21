package hypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta/tombstone"
)

// ErrTombstoned reports an entrypoint that met a deleting-phase entity:
// resources are partially gone and the operation must not touch the record.
var ErrTombstoned = errors.New("vm is being deleted")

// vmCleanup is the vms-namespace tombstone payload: everything teardown
// needs once the record is gone.
type vmCleanup struct {
	Name   string `json:"name,omitempty"`
	RunDir string `json:"run_dir,omitempty"`
	LogDir string `json:"log_dir,omitempty"`
}

func (b *Backend) tombstones() *tombstone.Table {
	return tombstone.NewTable(b.Meta, b.NS)
}

// deleteVMProtocol runs the §5 phase protocol for one VM under its held ops
// lock: lease with full payload → deleting → slow teardown → fenced finalize.
// A crash at any point leaves a phase-directed tombstone a later worker
// resumes.
func (b *Backend) deleteVMProtocol(ctx context.Context, id string, rec *VMRecord) error {
	ts := b.tombstones()
	cl := vmCleanup{Name: rec.Config.Name, RunDir: rec.RunDir, LogDir: rec.LogDir}
	cleanup, err := tombstone.MarshalCleanup(cl)
	if err != nil {
		return err
	}
	var leaseID string
	if err := b.update(ctx, func(t *vmTx) error {
		if r, err := t.Get(id); err != nil {
			return err
		} else if r == nil {
			return ErrNotFound
		}
		var resumed *tombstone.Record
		var err error
		leaseID, resumed, err = ts.Acquire(ctx, t.w, id, func() (tombstone.Payload, error) {
			return tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup}, nil
		})
		if err != nil || resumed == nil {
			return err
		}
		// A dead owner's lease: resume with ITS payload, not a re-derivation.
		return json.Unmarshal(resumed.Payload.Cleanup, &cl)
	}); err != nil {
		return err
	}
	if err := b.update(ctx, func(t *vmTx) error {
		return ts.MarkDeleting(ctx, t.w, id, leaseID)
	}); err != nil {
		return err
	}
	return b.finishVMTeardown(ctx, id, leaseID, cl)
}

// finishVMTeardown runs the slow cleanup outside any transaction, then the
// fenced finalize that deletes record, name and tombstone together.
func (b *Backend) finishVMTeardown(ctx context.Context, id, leaseID string, cl vmCleanup) error {
	if err := b.cleanupNetwork(ctx, id); err != nil {
		return fmt.Errorf("vm %s network teardown (tombstone kept, retry or gc resumes): %w", id, err)
	}
	if err := RemoveVMDirs(cl.RunDir, cl.LogDir); err != nil {
		return fmt.Errorf("cleanup VM dirs (tombstone kept, retry or gc resumes): %w", err)
	}
	ts := b.tombstones()
	err := b.update(ctx, func(t *vmTx) error {
		if err := t.Del(id); err != nil {
			return err
		}
		if err := t.NameDelIfOwned(cl.Name, id); err != nil {
			return err
		}
		return ts.Finalize(ctx, t.w, id, leaseID)
	})
	if errors.Is(err, tombstone.ErrLost) {
		// Another worker recovered and finalized after this one's lease died.
		return nil
	}
	return err
}

// recoverVMTombstone drives id's tombstone to completion under the held ops
// lock: leased rolls back (record stays live), deleting rolls forward from
// the payload. done reports the entity was finalized (record gone).
func (b *Backend) recoverVMTombstone(ctx context.Context, id string) (done bool, err error) { //nolint:unparam // done is asserted by the protocol gates
	ts := b.tombstones()
	var (
		rec     *tombstone.Record
		leaseID string
	)
	if err := b.update(ctx, func(t *vmTx) error {
		var err error
		rec, leaseID, err = ts.Resume(ctx, t.w, id)
		return err
	}); err != nil {
		return false, err
	}
	if rec == nil || rec.Phase == tombstone.PhaseLeased {
		return false, nil
	}
	var cl vmCleanup
	if err := json.Unmarshal(rec.Payload.Cleanup, &cl); err != nil {
		return false, fmt.Errorf("tombstone %s payload: %w", id, err)
	}
	if err := b.finishVMTeardown(ctx, id, leaseID, cl); err != nil {
		return false, err
	}
	log.WithFunc(b.Typ+".recoverVMTombstone").Warnf(ctx, "rolled forward interrupted delete of VM %s", id)
	return true, nil
}

// EntryGuardLoad is EntryGuard returning the VM's record from the guard's own
// transaction, sparing lock-held entry paths a second whole-namespace read.
func (b *Backend) EntryGuardLoad(ctx context.Context, id string) (VMRecord, error) {
	rec, err := b.entryGuard(ctx, id)
	if err != nil {
		return VMRecord{}, err
	}
	if rec == nil {
		return VMRecord{}, fmt.Errorf("%q not found", id)
	}
	return *rec, nil
}

func (b *Backend) entryGuard(ctx context.Context, id string) (*VMRecord, error) {
	var rec *VMRecord
	err := b.update(ctx, func(t *vmTx) error {
		if err := b.guardVMTombstone(ctx, t, id); err != nil {
			return err
		}
		var getErr error
		rec, getErr = t.Get(id)
		return getErr
	})
	if !errors.Is(err, ErrTombstoned) {
		return rec, err
	}
	if _, rerr := b.recoverVMTombstone(ctx, id); rerr != nil {
		return nil, fmt.Errorf("vm %s: recover interrupted delete: %w", id, rerr)
	}
	return nil, fmt.Errorf("vm %s was partially deleted; recovery finished the removal: %w", id, ErrNotFound)
}

// guardVMTombstone refuses tombstoned VMs inside the entrypoint's own
// transaction: leased rolls back in place, deleting reports ErrTombstoned.
func (b *Backend) guardVMTombstone(ctx context.Context, t *vmTx, id string) error {
	ts := b.tombstones()
	rec, err := ts.Get(ctx, t.w, id)
	if err != nil || rec == nil {
		return err
	}
	if rec.Phase == tombstone.PhaseLeased {
		taken, err := ts.TakeOver(ctx, t.w, id)
		if err != nil {
			return err
		}
		return ts.Rollback(ctx, t.w, id, taken.LeaseID)
	}
	return fmt.Errorf("vm %s: %w", id, ErrTombstoned)
}
