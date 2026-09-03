package hypervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/types"
)

func TestDeleteProtocolFullRun(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	rec := seedProtoVM(t, b, "vmproto1")

	var tornDown []string
	b.SetNetwork(stubNetwork{cleanup: func(_ context.Context, id string) error {
		tornDown = append(tornDown, id)
		return nil
	}})
	if err := b.deleteVMProtocol(ctx, "vmproto1", rec); err != nil {
		t.Fatalf("protocol: %v", err)
	}
	if len(tornDown) != 1 || tornDown[0] != "vmproto1" {
		t.Fatalf("network teardown calls: %v", tornDown)
	}
	if _, err := os.Stat(rec.RunDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run dir survived: %v", err)
	}
	if _, err := b.LoadRecord(ctx, "vmproto1"); err == nil {
		t.Fatal("record survived finalize")
	}
	if err := b.view(ctx, func(tx *vmTx) error {
		if _, ok, err := tx.NameGet("proto-vmproto1"); err != nil {
			return err
		} else if ok {
			t.Fatal("name survived finalize")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteProtocolTeardownFailureKeepsTombstoneThenConverges(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	rec := seedProtoVM(t, b, "vmproto2")

	boom := errors.New("cni down")
	failing := func(context.Context, string) error { return boom }
	b.SetNetwork(stubNetwork{cleanup: failing})
	if err := b.deleteVMProtocol(ctx, "vmproto2", rec); !errors.Is(err, boom) {
		t.Fatalf("want teardown failure surfaced, got %v", err)
	}

	if _, err := b.LoadRecord(ctx, "vmproto2"); err != nil {
		t.Fatalf("record must stay live through deleting: %v", err)
	}
	b.SetNetwork(stubNetwork{})
	done, err := b.recoverVMTombstone(ctx, "vmproto2")
	if err != nil || !done {
		t.Fatalf("roll forward: done=%v err=%v", done, err)
	}
	if _, err := b.LoadRecord(ctx, "vmproto2"); err == nil {
		t.Fatal("record survived recovery finalize")
	}
}

func TestRecoverLeasedRollsBack(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	rec := seedProtoVM(t, b, "vmproto3")

	cleanup, err := tombstone.MarshalCleanup(vmCleanup{Name: rec.Config.Name, RunDir: rec.RunDir, LogDir: rec.LogDir})
	if err != nil {
		t.Fatal(err)
	}
	ts := b.tombstones()
	if err := b.update(ctx, func(tx *vmTx) error {
		_, err := ts.Lease(ctx, tx.w, "vmproto3", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	done, err := b.recoverVMTombstone(ctx, "vmproto3")
	if err != nil || done {
		t.Fatalf("leased must roll back, not finalize: done=%v err=%v", done, err)
	}
	if _, err := b.LoadRecord(ctx, "vmproto3"); err != nil {
		t.Fatalf("record must survive a leased rollback: %v", err)
	}
	if _, err := os.Stat(rec.RunDir); err != nil {
		t.Fatalf("run dir must survive a leased rollback: %v", err)
	}
}

func TestTombstoneFencingABA(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	rec := seedProtoVM(t, b, "vmproto4")
	ts := b.tombstones()

	cleanup, err := tombstone.MarshalCleanup(vmCleanup{Name: rec.Config.Name, RunDir: rec.RunDir, LogDir: rec.LogDir})
	if err != nil {
		t.Fatal(err)
	}
	var leaseA string
	if err := b.update(ctx, func(tx *vmTx) error {
		leaseA, err = ts.Lease(ctx, tx.w, "vmproto4", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.update(ctx, func(tx *vmTx) error {
		return ts.MarkDeleting(ctx, tx.w, "vmproto4", leaseA)
	}); err != nil {
		t.Fatal(err)
	}

	done, err := b.recoverVMTombstone(ctx, "vmproto4")
	if err != nil || !done {
		t.Fatalf("recover: done=%v err=%v", done, err)
	}

	err = b.update(ctx, func(tx *vmTx) error {
		return ts.Finalize(ctx, tx.w, "vmproto4", leaseA)
	})
	if !errors.Is(err, tombstone.ErrLost) {
		t.Fatalf("A's stale finalize must lose its fence, got %v", err)
	}
}

func TestEntryGuardDisciplines(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	rec := seedProtoVM(t, b, "vmproto5")
	ts := b.tombstones()
	cleanup, err := tombstone.MarshalCleanup(vmCleanup{Name: rec.Config.Name, RunDir: rec.RunDir, LogDir: rec.LogDir})
	if err != nil {
		t.Fatal(err)
	}

	if err := b.update(ctx, func(tx *vmTx) error {
		_, err := ts.Lease(ctx, tx.w, "vmproto5", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := entryGuardOnly(ctx, b, "vmproto5"); err != nil {
		t.Fatalf("leased guard must roll back and proceed: %v", err)
	}

	var lease string
	if err := b.update(ctx, func(tx *vmTx) error {
		lease, err = ts.Lease(ctx, tx.w, "vmproto5", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.update(ctx, func(tx *vmTx) error {
		return ts.MarkDeleting(ctx, tx.w, "vmproto5", lease)
	}); err != nil {
		t.Fatal(err)
	}
	err = entryGuardOnly(ctx, b, "vmproto5")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting guard must refuse after recovery: %v", err)
	}
	if _, err := b.LoadRecord(ctx, "vmproto5"); err == nil {
		t.Fatal("record must be finalized by driven recovery")
	}
}

func TestLiveHolderKeepsLease(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	rec := seedProtoVM(t, b, "vmlive1")
	ts := b.tombstones()

	cleanup, err := tombstone.MarshalCleanup(vmCleanup{Name: rec.Config.Name, RunDir: rec.RunDir, LogDir: rec.LogDir})
	if err != nil {
		t.Fatal(err)
	}
	var leaseA string
	if err := b.update(ctx, func(tx *vmTx) error {
		leaseA, err = ts.Lease(ctx, tx.w, "vmlive1", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.update(ctx, func(tx *vmTx) error {
		return ts.MarkDeleting(ctx, tx.w, "vmlive1", leaseA)
	}); err != nil {
		t.Fatal(err)
	}

	unlock, err := b.LockVMOps(ctx, "vmlive1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if errs := b.gcRecover(ctx); len(errs) != 0 {
		t.Fatalf("gcRecover against a live holder must skip, got %v", errs)
	}

	var after *tombstone.Record
	if err := b.view(ctx, func(tx *vmTx) error {
		var err error
		after, err = ts.Get(ctx, tx.r, "vmlive1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after == nil || after.LeaseID != leaseA || after.Phase != tombstone.PhaseDeleting {
		t.Fatalf("live holder's lease disturbed: %+v (want lease %s deleting)", after, leaseA)
	}
	if _, err := os.Stat(rec.RunDir); err != nil {
		t.Fatalf("live holder's run dir touched: %v", err)
	}
}

func TestPrepareStartRefusesMidDeleting(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	var netTorn []string
	b.SetNetwork(stubNetwork{cleanup: func(_ context.Context, id string) error {
		netTorn = append(netTorn, id)
		return nil
	}})
	ctx := t.Context()
	rec := seedProtoVM(t, b, "vmentry1")
	ts := b.tombstones()

	cleanup, err := tombstone.MarshalCleanup(vmCleanup{Name: rec.Config.Name, RunDir: rec.RunDir, LogDir: rec.LogDir})
	if err != nil {
		t.Fatal(err)
	}
	var leaseA string
	if err := b.update(ctx, func(tx *vmTx) error {
		leaseA, err = ts.Lease(ctx, tx.w, "vmentry1", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.update(ctx, func(tx *vmTx) error {
		return ts.MarkDeleting(ctx, tx.w, "vmentry1", leaseA)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := b.PrepareStart(ctx, "vmentry1", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mid-deleting VM must refuse to boot with ErrNotFound, got %v", err)
	}
	if _, err := b.LoadRecord(ctx, "vmentry1"); err == nil {
		t.Fatal("record survived entry-driven recovery")
	}
	if _, err := os.Stat(rec.RunDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run dir survived entry-driven recovery: %v", err)
	}
	var after *tombstone.Record
	if err := b.view(ctx, func(tx *vmTx) error {
		var err error
		after, err = ts.Get(ctx, tx.r, "vmentry1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after != nil {
		t.Fatalf("tombstone survived entry-driven recovery: %+v", after)
	}
	if len(netTorn) != 1 || netTorn[0] != "vmentry1" {
		t.Fatalf("entry-driven recovery skipped the injected network cleanup: %v", netTorn)
	}
}

type stubNetwork struct {
	recover func(context.Context, *types.VM) error
	quiesce func(context.Context, *types.VM) error
	cleanup func(context.Context, string) error
}

func (s stubNetwork) Recover(ctx context.Context, vm *types.VM) error {
	if s.recover == nil {
		return nil
	}
	return s.recover(ctx, vm)
}

func (s stubNetwork) Quiesce(ctx context.Context, vm *types.VM) error {
	if s.quiesce == nil {
		return nil
	}
	return s.quiesce(ctx, vm)
}

func (s stubNetwork) Cleanup(ctx context.Context, vmID string) error {
	if s.cleanup == nil {
		return nil
	}
	return s.cleanup(ctx, vmID)
}

func seedProtoVM(t *testing.T, b *Backend, id string) *VMRecord {
	t.Helper()
	runDir, logDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(runDir, "cow.raw"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.ReserveVM(t.Context(), id, &types.VMConfig{Name: "proto-" + id}, nil, runDir, logDir); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := b.UpdateRecord(t.Context(), id, func(r *VMRecord) error {
		r.State = types.VMStateStopped
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := b.LoadRecord(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return &rec
}

func entryGuardOnly(ctx context.Context, b *Backend, id string) error {
	_, err := b.entryGuard(ctx, id)
	return err
}
