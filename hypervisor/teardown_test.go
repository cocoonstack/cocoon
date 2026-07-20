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

func TestDeleteProtocolFullRun(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	rec := seedProtoVM(t, b, "vmproto1")

	var tornDown []string
	teardown := func(_ context.Context, id string) error {
		tornDown = append(tornDown, id)
		return nil
	}
	if err := b.deleteVMProtocol(ctx, "vmproto1", rec, teardown); err != nil {
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
	if err := b.deleteVMProtocol(ctx, "vmproto2", rec, failing); !errors.Is(err, boom) {
		t.Fatalf("want teardown failure surfaced, got %v", err)
	}
	// Record stays live over a deleting tombstone; the retry rolls forward.
	if _, err := b.LoadRecord(ctx, "vmproto2"); err != nil {
		t.Fatalf("record must stay live through deleting: %v", err)
	}
	done, err := b.recoverVMTombstone(ctx, "vmproto2", nil)
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

	// Simulate a crash right after the lease transaction: insert the lease only.
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

	done, err := b.recoverVMTombstone(ctx, "vmproto3", nil)
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
	// Worker A leases and flips to deleting, then "dies".
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
	// Worker B recovers under a NEW lease and finalizes.
	done, err := b.recoverVMTombstone(ctx, "vmproto4", nil)
	if err != nil || !done {
		t.Fatalf("recover: done=%v err=%v", done, err)
	}
	// Replaying A's fenced finalize affects nothing.
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

	// leased: the guard rolls it back in place and the operation proceeds.
	if err := b.update(ctx, func(tx *vmTx) error {
		_, err := ts.Lease(ctx, tx.w, "vmproto5", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.entryGuard(ctx, "vmproto5"); err != nil {
		t.Fatalf("leased guard must roll back and proceed: %v", err)
	}

	// deleting: the guard drives recovery to completion and refuses.
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
	err = b.entryGuard(ctx, "vmproto5")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting guard must refuse after recovery: %v", err)
	}
	if _, err := b.LoadRecord(ctx, "vmproto5"); err == nil {
		t.Fatal("record must be finalized by driven recovery")
	}
}
