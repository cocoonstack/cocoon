package hypervisor

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
)

// vmCleanup is the vms-namespace tombstone payload: what teardown needs once the record is gone.
type vmCleanup struct {
	Name   string `json:"name,omitempty"`
	RunDir string `json:"run_dir,omitempty"`
	LogDir string `json:"log_dir,omitempty"`
}

// EntryGuardLoad returns the record from the guard's own transaction, so entry paths skip a second namespace read.
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

// RecoverTombstone drives id's tombstone to completion under the held ops lock.
func (b *Backend) RecoverTombstone(ctx context.Context, id string) (done bool, err error) {
	ts := b.tombstones()
	var (
		rec *tombstone.Record
		cl  vmCleanup
		vm  *VMRecord
	)
	if err := b.update(ctx, func(t *vmTx) error {
		var err error
		rec, err = ts.Recover(ctx, t.w, id, &cl)
		if err != nil || rec == nil {
			return err
		}
		vm, err = t.Get(id)
		return err
	}); err != nil {
		return false, err
	}
	if rec == nil {
		return false, nil
	}
	if err := b.finishVMTeardown(ctx, id, rec.LeaseID, cl); err != nil {
		return false, err
	}
	if vm != nil && vm.State != types.VMStateCreating {
		b.emitDeleteClose(ctx, id, shapeFromConfig(vm.Config), metering.ReasonStopCrash, hasOpenComputeInterval(vm))
	}
	log.WithFunc(b.Typ+".RecoverTombstone").Warnf(ctx, "rolled forward interrupted delete of VM %s", id)
	return true, nil
}

func (b *Backend) tombstones() *tombstone.Table {
	return tombstone.NewTable(b.NS)
}

// deleteVMProtocol runs lease, deleting, teardown and fenced finalize under the held ops lock; a crash leaves a tombstone a later worker resumes.
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

// finishVMTeardown runs the slow cleanup outside any transaction, then the fenced finalize.
func (b *Backend) finishVMTeardown(ctx context.Context, id, leaseID string, cl vmCleanup) error {
	if err := b.cleanupNetwork(ctx, id); err != nil {
		return fmt.Errorf("vm %s network teardown (tombstone kept, retry or gc resumes): %w", id, err)
	}
	if err := RemoveVMDirs(cl.RunDir, cl.LogDir); err != nil {
		return fmt.Errorf("cleanup VM dirs (tombstone kept, retry or gc resumes): %w", err)
	}
	b.removeCgroupScope(ctx, id)
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

// entryGuard keeps the common no-tombstone path off the single sqlite writer connection.
func (b *Backend) entryGuard(ctx context.Context, id string) (*VMRecord, error) {
	ts := b.tombstones()
	var (
		rec   *VMRecord
		tsRec *tombstone.Record
	)
	if err := b.view(ctx, func(t *vmTx) error {
		var err error
		if tsRec, err = ts.Get(ctx, t.r, id); err != nil || tsRec != nil {
			return err
		}
		rec, err = t.Get(id)
		return err
	}); err != nil {
		return nil, err
	}
	if tsRec == nil {
		return rec, nil
	}
	if tsRec.Phase != tombstone.PhaseLeased {
		if _, rerr := b.RecoverTombstone(ctx, id); rerr != nil {
			return nil, fmt.Errorf("vm %s: recover interrupted delete: %w", id, rerr)
		}
		return nil, fmt.Errorf("vm %s was partially deleted; recovery finished the removal: %w", id, ErrNotFound)
	}
	if err := b.update(ctx, func(t *vmTx) error {
		taken, err := ts.TakeOver(ctx, t.w, id)
		if err != nil {
			return err
		}
		if rbErr := ts.Rollback(ctx, t.w, id, taken.LeaseID); rbErr != nil {
			return rbErr
		}
		rec, err = t.Get(id)
		return err
	}); err != nil {
		return nil, err
	}
	return rec, nil
}
