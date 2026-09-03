package hypervisor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestStartSequenceFlipsRunningUnderLock(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-start"
	seedStoppedVMWithDirs(t, b, id)

	err := b.StartSequence(ctx, id, StartSpec{
		Launch: func(context.Context, *VMRecord, string) (int, error) { return 4242, nil },
	})
	if err != nil {
		t.Fatalf("StartSequence: %v", err)
	}
	loaded, err := b.LoadRecord(ctx, id)
	if err != nil {
		t.Fatalf("LoadRecord: %v", err)
	}
	if loaded.State != types.VMStateRunning {
		t.Errorf("state = %s, want running (flip must land inside the sequence)", loaded.State)
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].VMID != id {
		t.Fatalf("got %+v, want one compute.start for %s", entries, id)
	}
}

func TestStartSequenceRejectsNilNIC(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-nil-nic"
	seedStoppedVMWithDirs(t, b, id)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs[id].NetworkConfigs = []*types.NetworkConfig{nil}
		return nil
	}); err != nil {
		t.Fatalf("seed nil NIC: %v", err)
	}

	err := b.StartSequence(ctx, id, StartSpec{
		Launch: func(context.Context, *VMRecord, string) (int, error) {
			t.Fatal("Launch must not run with a nil NIC in the record")
			return 0, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "network invariants violated") {
		t.Fatalf("err = %v, want network invariants violation", err)
	}
	loaded, _ := b.LoadRecord(ctx, id)
	if loaded.State != types.VMStateError {
		t.Errorf("state = %s, want error", loaded.State)
	}
}

func TestStartSequenceLaunchFailureSchedulesNetworkConvergence(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx, cancel := context.WithCancel(t.Context())
	const id = "vm-launch-fail"
	seedStoppedVMWithDirs(t, b, id)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs[id].NetSetup = types.NetSetup{
			NetBackend:     types.BackendCNI,
			NetworkConfigs: []*types.NetworkConfig{{}},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	quiesced := 0
	b.SetNetwork(stubNetwork{quiesce: func(context.Context, *types.VM) error {
		quiesced++
		return nil
	}})

	err := b.StartSequence(ctx, id, StartSpec{
		Launch: func(context.Context, *VMRecord, string) (int, error) {
			cancel()
			return 0, errors.New("launch canceled")
		},
	})
	if err == nil {
		t.Fatal("expected launch failure")
	}
	rec := recordOf(t, b, id)
	if rec.State != types.VMStateError || !rec.QuiescePending {
		t.Fatalf("state = %s, pending = %v; want error/true", rec.State, rec.QuiescePending)
	}
	if err := b.ConvergeDead(t.Context(), id, rec.TransitionGeneration, timeNow()); err != nil {
		t.Fatalf("ConvergeDead: %v", err)
	}
	after := recordOf(t, b, id)
	if quiesced != 1 || after.QuiescePending {
		t.Fatalf("quiesced = %d, pending = %v; want 1/false", quiesced, after.QuiescePending)
	}
}

func seedStoppedVMWithDirs(t *testing.T, b *Backend, id string) {
	t.Helper()
	seedVMRecord(t, b, id, 1, 1<<30, 10<<30, true)
	dir := shortTempDir(t)
	if err := b.dbUpdate(t.Context(), func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateStopped
		idx.VMs[id].RunDir = dir
		idx.VMs[id].LogDir = dir
		return nil
	}); err != nil {
		t.Fatalf("seed dirs: %v", err)
	}
}
