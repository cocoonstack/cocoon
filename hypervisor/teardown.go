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

// NetTeardown releases a VM's host networking; the command layer supplies it
// so record deletion and network teardown share one VM lock (design §5).
type NetTeardown func(ctx context.Context, vmID string) error

func (b *Backend) tombstones() *tombstone.Table {
	return tombstone.NewTable(b.Meta, b.NS)
}

// deleteVMProtocol runs the §5 phase protocol for one VM under its held ops
// lock: lease with full payload → deleting → slow teardown → fenced finalize.
// A crash at any point leaves a phase-directed tombstone a later worker
// resumes.
func (b *Backend) deleteVMProtocol(ctx context.Context, id string, rec *VMRecord, teardown NetTeardown) error {
	ts := b.tombstones()
	cleanup, err := tombstone.MarshalCleanup(vmCleanup{Name: rec.Config.Name, RunDir: rec.RunDir, LogDir: rec.LogDir})
	if err != nil {
		return err
	}
	var (
		leaseID string
		cl      = vmCleanup{Name: rec.Config.Name, RunDir: rec.RunDir, LogDir: rec.LogDir}
	)
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
	return b.finishVMTeardown(ctx, id, leaseID, cl, teardown)
}

// finishVMTeardown runs the slow cleanup outside any transaction, then the
// fenced finalize that deletes record, name and tombstone together.
func (b *Backend) finishVMTeardown(ctx context.Context, id, leaseID string, cl vmCleanup, teardown NetTeardown) error {
	if teardown != nil {
		if err := teardown(ctx, id); err != nil {
			return fmt.Errorf("vm %s network teardown (tombstone kept, retry or gc resumes): %w", id, err)
		}
	}
	if err := RemoveVMDirs(cl.RunDir, cl.LogDir); err != nil {
		return fmt.Errorf("cleanup VM dirs (tombstone kept, retry or gc resumes): %w", err)
	}
	ts := b.tombstones()
	err := b.update(ctx, func(t *vmTx) error {
		if err := t.Del(id); err != nil {
			return err
		}
		if cl.Name != "" {
			if cur, ok, err := t.NameGet(cl.Name); err != nil {
				return err
			} else if ok && cur == id {
				if err := t.NameDel(cl.Name); err != nil {
					return err
				}
			}
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
func (b *Backend) recoverVMTombstone(ctx context.Context, id string, teardown NetTeardown) (done bool, err error) { //nolint:unparam // done is asserted by the protocol gates
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
	if err := b.finishVMTeardown(ctx, id, leaseID, cl, teardown); err != nil {
		return false, err
	}
	log.WithFunc(b.Typ+".recoverVMTombstone").Warnf(ctx, "rolled forward interrupted delete of VM %s", id)
	return true, nil
}

// EntryGuard enforces the entrypoint discipline under a held ops lock:
// roll a leased tombstone back in place; drive a deleting one to completion —
// including the injected network cleanup — and refuse the operation.
func (b *Backend) EntryGuard(ctx context.Context, id string) error {
	err := b.update(ctx, func(t *vmTx) error { return b.guardVMTombstone(ctx, t, id) })
	if !errors.Is(err, ErrTombstoned) {
		return err
	}
	if _, rerr := b.recoverVMTombstone(ctx, id, b.NetCleanup); rerr != nil {
		return fmt.Errorf("vm %s: recover interrupted delete: %w", id, rerr)
	}
	return fmt.Errorf("vm %s was partially deleted; recovery finished the removal: %w", id, ErrNotFound)
}

// guardVMTombstone is the entrypoint discipline (design §5): inside the
// same transaction that re-reads the record under the entity lock, refuse to
// operate on a tombstoned VM. leased tombstones roll back in place; deleting
// ones make the caller drive recovery and fail with ErrTombstoned.
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
