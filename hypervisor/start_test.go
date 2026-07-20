package hypervisor

import (
	"context"
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

// seedStoppedVMWithDirs seeds a stopped record whose run/log dirs exist (PrepareStart requires them).
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
