package hypervisor

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/metering"
	meteringcapture "github.com/cocoonstack/cocoon/metering/capture"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestDeleteAllStoppedVM(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-del"
	seedVMRecord(t, b, id, 2, 1<<30, 10<<30, true)
	runDir, logDir := shortTempDir(t), shortTempDir(t)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateStopped
		idx.VMs[id].RunDir = runDir
		idx.VMs[id].LogDir = logDir
		return nil
	}); err != nil {
		t.Fatalf("seed dirs: %v", err)
	}

	deleted, err := b.DeleteAll(ctx, []string{id}, false, func(context.Context, string) error {
		t.Fatal("stopLocked must not run for a stopped VM")
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != id {
		t.Fatalf("deleted = %v, want [%s]", deleted, id)
	}
	if _, err := b.LoadRecord(ctx, id); err == nil {
		t.Error("record should be gone after delete")
	}
	for _, dir := range []string{runDir, logDir} {
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Errorf("dir %s should be removed, stat err: %v", dir, statErr)
		}
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Kind != metering.KindVMStorageStop {
		t.Fatalf("got %+v, want one storage.stop (no open compute interval)", entries)
	}
}

func TestStopAfterUnexpectedExitIsIdempotent(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-stop-race"
	seedStoppedVMWithDirs(t, b, id)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		r := idx.VMs[id]
		r.TransitionGeneration = 7
		r.LastTransitionReason = types.TransitionUnexpectedExit
		r.QuiescePending = true
		r.NetSetup = types.NetSetup{
			NetBackend:     types.BackendCNI,
			NetworkConfigs: []*types.NetworkConfig{{}},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed unexpected exit: %v", err)
	}
	quiesced := 0
	b.SetNetwork(stubNetwork{quiesce: func(context.Context, *types.VM) error {
		quiesced++
		return nil
	}})

	if err := b.StopOneLocked(ctx, id, StopSpec{}); err != nil {
		t.Fatalf("StopOneLocked: %v", err)
	}
	rec := recordOf(t, b, id)
	if rec.TransitionGeneration != 7 || rec.LastTransitionReason != types.TransitionUnexpectedExit {
		t.Fatalf("generation = %d, reason = %s; want 7/unexpected-exit", rec.TransitionGeneration, rec.LastTransitionReason)
	}
	if quiesced != 1 || rec.QuiescePending {
		t.Fatalf("quiesced = %d, pending = %v; want 1/false", quiesced, rec.QuiescePending)
	}
}

func TestStopStaleStoppedRecordWithLiveVMMStillTransitions(t *testing.T) {
	b, id := newHibernateTestVM(t)
	ctx := t.Context()
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		r := idx.VMs[id]
		r.State = types.VMStateStopped
		r.TransitionGeneration = 7
		r.NetSetup = types.NetSetup{
			NetBackend:     types.BackendCNI,
			NetworkConfigs: []*types.NetworkConfig{{}},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed stale stopped record: %v", err)
	}
	quiesced := 0
	b.SetNetwork(stubNetwork{quiesce: func(context.Context, *types.VM) error {
		quiesced++
		return nil
	}})

	err := b.StopOneLocked(ctx, id, StopSpec{Shutdown: func(ctx context.Context, rec *VMRecord, sockPath string, pid int) error {
		return utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, time.Second)
	}})
	if err != nil {
		t.Fatalf("StopOneLocked: %v", err)
	}
	rec := recordOf(t, b, id)
	if rec.TransitionGeneration != 8 || rec.LastTransitionReason != types.TransitionStopUser {
		t.Fatalf("generation = %d, reason = %s; want 8/stop-user", rec.TransitionGeneration, rec.LastTransitionReason)
	}
	if quiesced != 1 || rec.QuiescePending {
		t.Fatalf("quiesced = %d, pending = %v; want 1/false", quiesced, rec.QuiescePending)
	}
}

// TestDeleteAllForceStopsUnderLock drives the full force path against a live
// stub VMM: the delete body holds the ops lock while stopLocked runs, so this
// deadlocks (and times out) if the stop variant re-takes the lock.
func TestDeleteAllForceStopsUnderLock(t *testing.T) {
	b, id := newHibernateTestVM(t)
	ctx := t.Context()

	if _, err := b.DeleteAll(ctx, []string{id}, false, func(context.Context, string) error { return nil }, nil); err == nil ||
		!strings.Contains(err.Error(), "running (force required)") {
		t.Fatalf("DeleteAll without force: %v, want running-requires-force error", err)
	}

	stopLocked := func(ctx context.Context, id string) error {
		return b.StopOneLocked(ctx, id, StopSpec{
			RuntimeFiles: []string{APISocketName},
			Shutdown: func(ctx context.Context, rec *VMRecord, sockPath string, pid int) error {
				return utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, time.Second)
			},
		})
	}
	deleted, err := b.DeleteAll(ctx, []string{id}, true, stopLocked, nil)
	if err != nil {
		t.Fatalf("DeleteAll --force: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != id {
		t.Fatalf("deleted = %v, want [%s]", deleted, id)
	}
	if _, err := b.LoadRecord(ctx, id); err == nil {
		t.Error("record should be gone after force delete")
	}
}

func TestDeleteAllRefusesLiveAPISocket(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-live-sock"
	seedVMRecord(t, b, id, 1, 1<<30, 10<<30, true)
	runDir := shortTempDir(t)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateStopped
		idx.VMs[id].RunDir = runDir
		idx.VMs[id].LogDir = runDir
		return nil
	}); err != nil {
		t.Fatalf("seed dirs: %v", err)
	}
	ln, err := net.Listen("unix", SocketPath(runDir))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck

	_, err = b.DeleteAll(ctx, []string{id}, true, func(context.Context, string) error { return nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "still responsive") {
		t.Fatalf("DeleteAll: %v, want live-socket refusal", err)
	}
	if _, loadErr := b.LoadRecord(ctx, id); loadErr != nil {
		t.Error("record must survive a refused delete")
	}
}

// shortTempDir is a /tmp-based TempDir: unix socket paths built under
// t.TempDir() can exceed the ~104-byte sockaddr cap (EINVAL on dial).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cocoon-test-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestDeleteMigratedVMClearsCleanupIntent(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-migrated"
	seedVMRecord(t, b, id, 1, 1<<30, 10<<30, true)
	runDir, logDir := shortTempDir(t), shortTempDir(t)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateStopped
		idx.VMs[id].RunDir = runDir
		idx.VMs[id].LogDir = logDir
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := b.DeleteAll(ctx, []string{id}, false, func(context.Context, string) error { return nil }, nil); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if err := b.dbRead(t.Context(), func(idx *VMIndex) error {
		if len(idx.OrphanDirs) != 0 {
			t.Fatalf("successful delete must clear its cleanup intent, got %v", idx.OrphanDirs)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatal("migrated run dir must be removed")
	}
}

// A forced delete of a running VM stops it first, and that stop already closes
// the compute interval; emitting again from the pre-stop record would bill two
// stops against one start.
func TestDeleteForceEmitsOneComputeStop(t *testing.T) {
	b, id := newHibernateTestVM(t)
	ctx := t.Context()
	rec := meteringcapture.New()
	b.Metering = rec
	if err := b.BatchMarkStarted(ctx, []string{id}); err != nil {
		t.Fatalf("BatchMarkStarted: %v", err)
	}

	stopLocked := func(ctx context.Context, id string) error {
		return b.StopOneLocked(ctx, id, StopSpec{
			RuntimeFiles: []string{APISocketName},
			Shutdown:     func(context.Context, *VMRecord, string, int) error { return nil },
		})
	}
	if _, err := b.DeleteAll(ctx, []string{id}, true, stopLocked, nil); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}

	stops := 0
	for _, e := range rec.Entries() {
		if e.Kind == metering.KindVMComputeStop {
			stops++
		}
	}
	if stops != 1 {
		t.Errorf("got %d vm.compute.stop for one start, want 1: %+v", stops, rec.Entries())
	}
}
