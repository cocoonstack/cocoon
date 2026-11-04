package hypervisor

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/types"
)

func TestStartSequenceFlipsRunningUnderLock(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-start"
	seedStoppedVMWithDirs(t, b, id)

	err := b.StartSequence(ctx, id, nil, StartSpec{
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

	err := b.StartSequence(ctx, id, nil, StartSpec{
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

	err := b.StartSequence(ctx, id, nil, StartSpec{
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

func TestWaitForSocketFailsFastOnAReapedProcess(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start probe: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	const timeout = 2 * time.Second
	start := time.Now()
	err := WaitForSocket(t.Context(), filepath.Join(t.TempDir(), "api.sock"), pid, timeout, "sh")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "exited before socket was ready") {
		t.Fatalf("err = %v, want exited before socket was ready", err)
	}
	if elapsed > timeout/2 {
		t.Fatalf("elapsed = %s, want a fast fail well inside %s", elapsed, timeout)
	}
}

func TestStartSequenceConvergesALiveVMMInsteadOfLaunching(t *testing.T) {
	convergeErr := errors.New("vmm busy")
	for _, want := range []error{nil, convergeErr} {
		b, id := newHibernateTestVM(t)
		if err := b.UpdateRecord(t.Context(), id, func(rec *VMRecord) error {
			rec.State = types.VMStateCreated
			return nil
		}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		converged := false
		err := b.StartSequence(t.Context(), id, nil, StartSpec{
			Launch: func(context.Context, *VMRecord, string) (int, error) {
				t.Fatal("a live VMM must not be launched again")
				return 0, nil
			},
			ConvergeRunning: func(context.Context, *VMRecord, string, int) error {
				if rec := recordOf(t, b, id); rec.State != types.VMStateCreated {
					t.Errorf("state before convergence = %s, want created", rec.State)
				}
				converged = true
				return want
			},
		})
		if !errors.Is(err, want) || !converged {
			t.Errorf("StartSequence: %v converged=%v, want %v and a converge", err, converged, want)
		}
		state := types.VMStateRunning
		if want != nil {
			state = types.VMStateCreated
		}
		if rec := recordOf(t, b, id); rec.State != state {
			t.Errorf("state after convergence = %s, want %s", rec.State, state)
		}
	}
}

func seedStoppedVMWithDirs(t *testing.T, b *Backend, id string) string {
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
	return dir
}
