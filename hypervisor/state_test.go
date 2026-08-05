package hypervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta/contracttest"
	"github.com/cocoonstack/cocoon/metering"
	meteringcapture "github.com/cocoonstack/cocoon/metering/capture"
	"github.com/cocoonstack/cocoon/types"
)

func TestBatchMarkStartedEmitsComputeStart(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 2, 4<<30, 10<<30, false)

	if err := b.BatchMarkStarted(ctx, []string{"vm1"}); err != nil {
		t.Fatalf("BatchMarkStarted: %v", err)
	}
	entries := rec.Entries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Kind != metering.KindVMComputeStart {
		t.Errorf("got kind %q, want %q", e.Kind, metering.KindVMComputeStart)
	}
	if e.Reason != metering.ReasonBoot {
		t.Errorf("got reason %q, want boot for first-time start", e.Reason)
	}
	if e.VMID != "vm1" || e.Hypervisor != "test-hv" {
		t.Errorf("identity wrong: %+v", e)
	}
	if e.Shape.CPU != 2 || e.Shape.MemBytes != 4<<30 || e.Shape.StorageBytes != 10<<30 {
		t.Errorf("shape wrong: %+v", e.Shape)
	}
}

func TestBatchMarkStartedReasonRestartWhenAlreadyBooted(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)

	if err := b.BatchMarkStarted(ctx, []string{"vm1"}); err != nil {
		t.Fatalf("BatchMarkStarted: %v", err)
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Reason != metering.ReasonRestart {
		t.Errorf("got %+v, want one entry with reason restart", entries)
	}
}

func TestUpdateStatesEmitsOnlyOnRunningToStopped(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)

	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates(stopped from created): %v", err)
	}
	if got := rec.Entries(); len(got) != 0 {
		t.Errorf("Created→Stopped emitted %d; want 0", len(got))
	}

	if err := b.BatchMarkStarted(ctx, []string{"vm1"}); err != nil {
		t.Fatalf("BatchMarkStarted: %v", err)
	}
	rec.Reset()

	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates(stopped): %v", err)
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Kind != metering.KindVMComputeStop || entries[0].Reason != metering.ReasonStopUser {
		t.Fatalf("Running→Stopped: got %+v, want one compute.stop reason=user", entries)
	}

	if err := b.BatchMarkStarted(ctx, []string{"vm1"}); err != nil {
		t.Fatalf("BatchMarkStarted (relaunch): %v", err)
	}
	rec.Reset()
	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateError); err != nil {
		t.Fatalf("UpdateStates(error): %v", err)
	}
	if got := rec.Entries(); len(got) != 0 {
		t.Errorf("Running→Error must not emit; got %d entries", len(got))
	}
}

func TestUpdateStatesRunningIsRejected(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, false)
	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateRunning); err == nil {
		t.Fatal("UpdateStates(Running) must return an error to steer callers to BatchMarkStarted")
	}
}

func TestPrepareStartClosesIntervalAfterMarkError(t *testing.T) {
	// Running→Error must leave the interval open (UpdateStates(Error) doesn't write StoppedAt). The next PrepareStart confirms the process is dead and closes the interval.
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 2<<30, 20<<30)
	dir := t.TempDir()
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs["vm1"].RunDir = dir
		idx.VMs["vm1"].LogDir = dir
		return nil
	}); err != nil {
		t.Fatalf("set dirs: %v", err)
	}

	b.MarkError(ctx, "vm1")
	if got := rec.Entries(); len(got) != 0 {
		t.Fatalf("MarkError emitted %d; want 0", len(got))
	}
	loaded, _ := b.LoadRecord(ctx, "vm1")
	if loaded.StoppedAt != nil {
		t.Errorf("MarkError must not write StoppedAt; got %v", loaded.StoppedAt)
	}

	prep, err := b.PrepareStart(ctx, "vm1", nil)
	if err != nil {
		t.Fatalf("PrepareStart: %v", err)
	}
	if prep == nil {
		t.Fatal("PrepareStart returned nil")
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Kind != metering.KindVMComputeStop || entries[0].Reason != metering.ReasonStopCrash {
		t.Fatalf("got %+v, want one compute.stop reason=stop-crash", entries)
	}
}

func TestStopAfterMarkErrorEmitsComputeStop(t *testing.T) {
	// Running→Error→Stopped: MarkError leaves the interval open; the recovery stop confirms the process is dead and must close it.
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 2<<30, 20<<30)
	b.MarkError(ctx, "vm1")
	rec.Reset()

	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates(stopped): %v", err)
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Kind != metering.KindVMComputeStop || entries[0].Reason != metering.ReasonStopUser {
		t.Fatalf("got %+v, want one compute.stop reason=user", entries)
	}
	loaded, _ := b.LoadRecord(ctx, "vm1")
	if loaded.StoppedAt == nil {
		t.Error("StoppedAt nil after Stopped transition")
	}
}

func TestDeleteForceClosesIntervalAfterMarkError(t *testing.T) {
	// rm --force on an Error VM with a still-open interval must emit compute.stop, not just storage.stop.
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 2<<30, 20<<30)
	b.MarkError(ctx, "vm1")
	rec.Reset()

	loaded, _ := b.LoadRecord(ctx, "vm1")
	if !hasOpenComputeInterval(&loaded) {
		t.Fatal("interval should still be open after MarkError")
	}

	b.emitDeleteClose(ctx, "vm1", shapeFromConfig(loaded.Config), metering.ReasonStopCrash, hasOpenComputeInterval(&loaded))
	entries := rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (compute.stop + storage.stop)", len(entries))
	}
}

func TestPrepareStartClosesStaleInterval(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 2<<30, 20<<30)
	dir := t.TempDir()
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs["vm1"].RunDir = dir
		idx.VMs["vm1"].LogDir = dir
		return nil
	}); err != nil {
		t.Fatalf("set dirs: %v", err)
	}
	rec.Reset()

	prep, err := b.PrepareStart(ctx, "vm1", nil)
	if err != nil {
		t.Fatalf("PrepareStart: %v", err)
	}
	if prep == nil {
		t.Fatal("PrepareStart returned nil (treated as already-running)")
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Kind != metering.KindVMComputeStop || entries[0].Reason != metering.ReasonStopCrash {
		t.Fatalf("got %+v, want one compute.stop reason=stop-crash", entries)
	}
	loaded, err := b.LoadRecord(ctx, "vm1")
	if err != nil {
		t.Fatalf("LoadRecord: %v", err)
	}
	if loaded.State != types.VMStateStopped {
		t.Errorf("State=%s, want Stopped", loaded.State)
	}
	if loaded.StoppedAt == nil {
		t.Error("StoppedAt nil")
	}
}

func TestFinalizeCloneEmitsCloneEntries(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 2, 2<<30, 20<<30, false)

	info := &types.VM{
		ID:         "vm1",
		Hypervisor: b.Typ,
		State:      types.VMStateRunning,
		Config:     types.VMConfig{Config: types.Config{CPU: 2, Memory: 2 << 30, Storage: 20 << 30}},
	}
	if err := b.FinalizeClone(ctx, "vm1", info, nil, nil, "snap-source"); err != nil {
		t.Fatalf("FinalizeClone: %v", err)
	}
	entries := rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (storage.start + compute.start)", len(entries))
	}
	for _, e := range entries {
		if e.Reason != metering.ReasonClone {
			t.Errorf("kind %s reason %q, want clone", e.Kind, e.Reason)
		}
		if e.SourceSnapshotID != "snap-source" {
			t.Errorf("kind %s source_snapshot_id %q, want snap-source", e.Kind, e.SourceSnapshotID)
		}
	}
	if entries[0].Kind != metering.KindVMStorageStart || entries[1].Kind != metering.KindVMComputeStart {
		t.Errorf("ordering wrong: %s then %s", entries[0].Kind, entries[1].Kind)
	}
}

func TestDirectRestoreSequenceEmitsComputeStopThenTransition(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 2<<30, 20<<30)

	newCfg := &types.VMConfig{Config: types.Config{CPU: 4, Memory: 4 << 30, Storage: 30 << 30}}
	spec := DirectRestoreSpec{
		VMCfg:            newCfg,
		SrcDir:           t.TempDir(),
		SourceSnapshotID: "snap-src",
		Preflight:        func(string, *VMRecord) error { return nil },
		Kill:             func(context.Context, string, *VMRecord) error { return nil },
		Populate:         func(*VMRecord, string) error { return nil },
		AfterExtract: func(_ context.Context, vmID string, vmCfg *types.VMConfig, _ *VMRecord) (*types.VM, error) {
			return &types.VM{ID: vmID, Hypervisor: b.Typ, State: types.VMStateRunning, Config: *vmCfg}, nil
		},
	}
	if _, err := b.DirectRestoreSequence(ctx, "vm1", spec); err != nil {
		t.Fatalf("DirectRestoreSequence: %v", err)
	}

	entries := rec.Entries()
	// compute.stop on kill; storage.stop + storage.start + compute.start on success.
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}
	wantOrder := []metering.Kind{
		metering.KindVMComputeStop,
		metering.KindVMStorageStop, metering.KindVMStorageStart, metering.KindVMComputeStart,
	}
	for i, want := range wantOrder {
		if entries[i].Kind != want {
			t.Errorf("entries[%d].Kind = %s, want %s", i, entries[i].Kind, want)
		}
		if entries[i].Reason != metering.ReasonRestore {
			t.Errorf("entries[%d].Reason = %q, want restore", i, entries[i].Reason)
		}
		if entries[i].SourceSnapshotID != "snap-src" {
			t.Errorf("entries[%d].SourceSnapshotID = %q, want snap-src", i, entries[i].SourceSnapshotID)
		}
	}
	// compute.stop and storage.stop carry the old shape; the open pair carries the new shape.
	for i := range 2 {
		if entries[i].Shape.CPU != 2 {
			t.Errorf("close entry %d cpu=%d, want 2 (old shape)", i, entries[i].Shape.CPU)
		}
	}
	for i := 2; i < 4; i++ {
		if entries[i].Shape.CPU != 4 {
			t.Errorf("open entry %d cpu=%d, want 4 (new shape)", i, entries[i].Shape.CPU)
		}
	}
}

func TestDirectRestoreSequenceEmitsOnlyComputeStopOnPopulateFailure(t *testing.T) {
	// Storage must stay open when restore fails after kill — the on-disk files
	// are still the old shape and vm rm will close it later with reason vm-rm.
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 2<<30, 20<<30)

	spec := DirectRestoreSpec{
		VMCfg:            &types.VMConfig{Config: types.Config{CPU: 4, Memory: 4 << 30, Storage: 30 << 30}},
		SrcDir:           t.TempDir(),
		SourceSnapshotID: "snap-src",
		Preflight:        func(string, *VMRecord) error { return nil },
		Kill:             func(context.Context, string, *VMRecord) error { return nil },
		Populate:         func(*VMRecord, string) error { return fmt.Errorf("populate boom") },
		AfterExtract: func(_ context.Context, _ string, _ *types.VMConfig, _ *VMRecord) (*types.VM, error) {
			t.Fatal("AfterExtract should not run when Populate fails")
			return nil, nil
		},
	}
	if _, err := b.DirectRestoreSequence(ctx, "vm1", spec); err == nil {
		t.Fatal("expected error from populate failure")
	}
	entries := rec.Entries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (compute.stop only; storage stays open)", len(entries))
	}
	if entries[0].Kind != metering.KindVMComputeStop {
		t.Errorf("entries[0].Kind = %s, want compute.stop", entries[0].Kind)
	}
}

func TestStartAllOnlyEmitsForActuallyLaunched(t *testing.T) {
	// BatchMarkStarted skipping every r.State==Running silently drops vm-stale (DB Running, process dead, relaunched).
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm-stopped", 1, 1<<30, 10<<30, false)
	seedRunningVM(t, b, "vm-running", 1, 1<<30, 10<<30)
	seedRunningVM(t, b, "vm-stale", 2, 2<<30, 20<<30)

	// Mimics StartSequence's per-VM contract: only an actual launch flips state.
	startOne := func(ctx context.Context, id string) error {
		switch id {
		case "vm-stopped", "vm-stale":
			return b.BatchMarkStarted(ctx, []string{id})
		case "vm-running":
			return nil
		}
		return fmt.Errorf("unexpected id: %s", id)
	}

	succeeded, err := b.StartAll(ctx, []string{"vm-stopped", "vm-running", "vm-stale"}, startOne)
	if err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if len(succeeded) != 3 {
		t.Errorf("succeeded %v, want 3", succeeded)
	}

	entries := rec.Entries()
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (vm-stopped: start; vm-stale: stop-crash + start; vm-running: none)", len(entries))
	}
	byVM := map[string][]metering.Entry{}
	for _, e := range entries {
		byVM[e.VMID] = append(byVM[e.VMID], e)
	}
	if got := byVM["vm-running"]; len(got) != 0 {
		t.Errorf("vm-running emitted %d entries; want 0", len(got))
	}
	if got := byVM["vm-stopped"]; len(got) != 1 || got[0].Kind != metering.KindVMComputeStart || got[0].Reason != metering.ReasonBoot {
		t.Errorf("vm-stopped: got %+v, want 1× compute.start reason=boot", got)
	}
	stale := byVM["vm-stale"]
	if len(stale) != 2 {
		t.Fatalf("vm-stale: got %d entries, want 2 (stop-crash close + restart open)", len(stale))
	}
	if stale[0].Kind != metering.KindVMComputeStop || stale[0].Reason != metering.ReasonStopCrash {
		t.Errorf("vm-stale[0]: got kind=%s reason=%q, want compute.stop reason=stop-crash", stale[0].Kind, stale[0].Reason)
	}
	if stale[1].Kind != metering.KindVMComputeStart || stale[1].Reason != metering.ReasonRestart {
		t.Errorf("vm-stale[1]: got kind=%s reason=%q, want compute.start reason=restart", stale[1].Kind, stale[1].Reason)
	}
}

func TestFinalizeCreateEmitsStorageStart(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	// FinalizeCreate requires an existing placeholder.
	seedVMRecord(t, b, "vm1", 2, 2<<30, 20<<30, false)

	info := &types.VM{
		ID:         "vm1",
		Hypervisor: b.Typ,
		Config:     types.VMConfig{Config: types.Config{CPU: 2, Memory: 2 << 30, Storage: 20 << 30}},
	}
	if err := b.FinalizeCreate(ctx, "vm1", info, nil, nil); err != nil {
		t.Fatalf("FinalizeCreate: %v", err)
	}
	entries := rec.Entries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Kind != metering.KindVMStorageStart || e.VMID != "vm1" || e.Reason != metering.ReasonBoot {
		t.Errorf("got %+v, want storage.start vm1 reason boot", e)
	}
	if e.Shape.StorageBytes != 20<<30 {
		t.Errorf("got storage %d, want %d", e.Shape.StorageBytes, int64(20<<30))
	}
}

func TestDeleteAfterErrorEmitsOnlyStorageStop(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 2, 2<<30, 20<<30, true)
	if err := b.BatchMarkStarted(ctx, []string{"vm1"}); err != nil {
		t.Fatalf("BatchMarkStarted: %v", err)
	}
	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates(stopped): %v", err)
	}
	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateError); err != nil {
		t.Fatalf("UpdateStates(error): %v", err)
	}
	rec.Reset()

	b.emitDeleteClose(ctx, "vm1", metering.Shape{CPU: 2, MemBytes: 2 << 30, StorageBytes: 20 << 30}, metering.ReasonStopCrash, false)
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Kind != metering.KindVMStorageStop {
		t.Fatalf("post-Error delete: got %+v, want one storage.stop", entries)
	}
}

func TestReconcileToRunningFromError(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 2<<30, 20<<30)
	b.MarkError(ctx, "vm1")
	rec.Reset()

	b.ReconcileToRunning(ctx, "vm1")

	loaded, err := b.LoadRecord(ctx, "vm1")
	if err != nil {
		t.Fatalf("LoadRecord: %v", err)
	}
	if loaded.State != types.VMStateRunning {
		t.Errorf("State=%s, want Running", loaded.State)
	}
	if loaded.StoppedAt != nil {
		t.Errorf("StoppedAt=%v, want nil", loaded.StoppedAt)
	}
	if !hasOpenComputeInterval(&loaded) {
		t.Error("compute interval should be open after reconcile")
	}
	if got := rec.Entries(); len(got) != 0 {
		t.Errorf("reconcile must not emit ledger entries; got %d", len(got))
	}
}

func TestReconcileToRunningOrphanLaunchEmitsStart(t *testing.T) {
	// BatchMarkStarted's DB write failed after a successful launch: process is alive but record is still Created (StartedAt=nil, no ledger compute.start). reconcile must emit a fresh start so a later stop has a matching open interval.
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 2, 2<<30, 20<<30, false)

	b.ReconcileToRunning(ctx, "vm1")

	loaded, _ := b.LoadRecord(ctx, "vm1")
	if loaded.State != types.VMStateRunning {
		t.Errorf("State=%s, want Running", loaded.State)
	}
	if !loaded.FirstBooted {
		t.Error("FirstBooted should be true after orphan reconcile")
	}
	if !hasOpenComputeInterval(&loaded) {
		t.Error("compute interval should be open after orphan reconcile")
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Kind != metering.KindVMComputeStart || entries[0].Reason != metering.ReasonBoot {
		t.Fatalf("got %+v, want one compute.start reason=boot", entries)
	}
}

func TestReconcileToRunningOrphanLaunchAfterFirstBoot(t *testing.T) {
	// Same orphan, but the VM was booted before: reconcile uses reason=restart.
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)

	b.ReconcileToRunning(ctx, "vm1")

	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Reason != metering.ReasonRestart {
		t.Fatalf("got %+v, want one compute.start reason=restart", entries)
	}
}

func TestReconcileToRunningIdempotent(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 2<<30, 20<<30)
	rec.Reset()

	b.ReconcileToRunning(ctx, "vm1")

	if got := rec.Entries(); len(got) != 0 {
		t.Errorf("idempotent reconcile must not emit; got %d", len(got))
	}
	loaded, _ := b.LoadRecord(ctx, "vm1")
	if loaded.State != types.VMStateRunning {
		t.Errorf("State=%s, want Running (unchanged)", loaded.State)
	}
}

func TestNewBackendNilRecorderDefaultsToNop(t *testing.T) {
	cfg := newDiskStubConfig(t)
	b, err := NewBackend("test-hv", cfg, nil, newTestMetaStore(t, "test-hv", cfg.indexFile, cfg.indexLock))
	if err != nil {
		t.Fatalf("NewBackend(rec=nil): %v", err)
	}
	if _, ok := b.Metering.(metering.NopRecorder); !ok {
		t.Fatalf("nil recorder should default to NopRecorder, got %T", b.Metering)
	}
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, false)
	if err := b.BatchMarkStarted(ctx, []string{"vm1"}); err != nil {
		t.Errorf("BatchMarkStarted with NopRecorder: %v", err)
	}
}

func TestPrepareStartRefusesQuarantined(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)

	b.QuarantineVM(ctx, "vm1", "partial snapshot merge")
	if _, err := b.PrepareStart(ctx, "vm1", nil); err == nil {
		t.Fatal("PrepareStart must refuse a quarantined VM")
	}

	// Stop's state flip must not lift the quarantine.
	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates: %v", err)
	}
	if _, err := b.PrepareStart(ctx, "vm1", nil); err == nil {
		t.Fatal("PrepareStart must refuse a quarantined VM after stop rewrote the state")
	}
}

func TestPrepareStartRefusesInterruptedRestore(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-staging"
	seedVMRecord(t, b, id, 1, 1<<30, 10<<30, true)
	runDir := t.TempDir()
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateStopped
		idx.VMs[id].RunDir = runDir
		idx.VMs[id].LogDir = runDir
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, restoreDirtyName), nil, 0o600); err != nil {
		t.Fatalf("mk tombstone: %v", err)
	}
	if _, err := b.PrepareStart(ctx, id, nil); err == nil {
		t.Fatal("PrepareStart must refuse a run dir with a restore-dirty tombstone")
	}
}

func TestPrepareStartRefusesCreating(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	if err := b.ReserveVM(ctx, "vm1", &types.VMConfig{Name: "n1"}, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("ReserveVM: %v", err)
	}
	if _, err := b.PrepareStart(ctx, "vm1", nil); err == nil {
		t.Fatal("PrepareStart must refuse a creating placeholder")
	}
}

// TestForcedRetryStateOps is the design §10 gate: the migrated
// UpdateStates/BatchMarkStarted run on a forced-retry engine (every closure
// executes twice) and must emit each metering entry exactly once.
func TestForcedRetryStateOps(t *testing.T) {
	const typ = "test-hv"
	dir := t.TempDir()
	rec := meteringcapture.New()
	b := &Backend{
		Typ:      typ,
		NS:       VMNamespaceName(typ),
		Conf:     meteringStubConfig{stubBackendConfig: stubBackendConfig{rootDir: dir}, vmRunRoot: dir},
		Meta:     contracttest.ForcedRetry(testNamespace(t, typ, dir)),
		Metering: rec,
	}
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 2, 1<<30, 10<<30, false)

	if err := b.BatchMarkStarted(ctx, []string{"vm1"}); err != nil {
		t.Fatalf("BatchMarkStarted under forced retry: %v", err)
	}
	if starts := rec.Entries(); len(starts) != 1 || starts[0].Kind != metering.KindVMComputeStart {
		t.Fatalf("want exactly one compute.start, got %+v", starts)
	}
	rec.Reset()

	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates under forced retry: %v", err)
	}
	if stops := rec.Entries(); len(stops) != 1 || stops[0].Kind != metering.KindVMComputeStop {
		t.Fatalf("want exactly one compute.stop, got %+v", stops)
	}
	loaded, err := b.LoadRecord(ctx, "vm1")
	if err != nil || loaded.State != types.VMStateStopped {
		t.Fatalf("state after forced-retry ops: %+v %v", loaded, err)
	}
}

// stubBackendConfig satisfies BackendConfig for tests that only exercise the
// metering wiring; unused methods panic so accidental dependence shows up loud.
type stubBackendConfig struct {
	rootDir   string
	indexFile string
	indexLock string
}

func (c stubBackendConfig) RootDirPath() string { return c.rootDir }

func (stubBackendConfig) BinaryName() string                  { return "stub-vmm" }
func (stubBackendConfig) PIDFileName() string                 { return "stub.pid" }
func (stubBackendConfig) TerminateGracePeriod() time.Duration { return time.Second }
func (stubBackendConfig) SocketWaitTimeout() time.Duration    { return time.Second }
func (stubBackendConfig) EffectivePoolSize() int              { return 1 }
func (stubBackendConfig) EnsureDirs() error                   { return nil }
func (stubBackendConfig) RunDir() string                      { panic("RunDir: not implemented in stub") }

func (stubBackendConfig) LogDir() string { panic("LogDir: not implemented in stub") }

func (stubBackendConfig) VMRunDir(string) string { panic("VMRunDir: not implemented in stub") }

func (stubBackendConfig) VMLogDir(string) string { panic("VMLogDir: not implemented in stub") }

func (c stubBackendConfig) CgroupParentDir() string { return filepath.Join(c.rootDir, "cgroup") }

func (stubBackendConfig) CgroupCPUFence() string { return "" }

// meteringStubConfig gives the metering stub a real VMRunDir so sequences
// can take the per-VM ops lock and MkdirTemp under it.
type meteringStubConfig struct {
	stubBackendConfig
	vmRunRoot string
}

func (c meteringStubConfig) VMRunDir(string) string { return c.vmRunRoot }

func (c meteringStubConfig) VMLogDir(string) string { return c.vmRunRoot }

func (c meteringStubConfig) RunDir() string { return c.vmRunRoot }

func (c meteringStubConfig) LogDir() string { return c.vmRunRoot }

func newDiskStubConfig(t *testing.T) stubBackendConfig {
	dir := t.TempDir()
	return stubBackendConfig{
		rootDir:   dir,
		indexFile: filepath.Join(dir, "index.json"),
		indexLock: filepath.Join(dir, "index.lock"),
	}
}

func newMeteringTestBackend(t *testing.T) (*Backend, *meteringcapture.Recorder) {
	t.Helper()
	const typ = "test-hv"
	dir := t.TempDir()
	store := testNamespace(t, typ, dir)
	rec := meteringcapture.New()
	return &Backend{
		Typ:      typ,
		NS:       VMNamespaceName(typ),
		Conf:     meteringStubConfig{stubBackendConfig: stubBackendConfig{rootDir: dir}, vmRunRoot: dir},
		Meta:     store,
		Metering: rec,
	}, rec
}

func seedVMRecord(t *testing.T, b *Backend, id string, cpu int, mem, storage int64, firstBooted bool) {
	t.Helper()
	if err := b.dbUpdate(t.Context(), func(idx *VMIndex) error {
		idx.VMs[id] = &VMRecord{
			VM: types.VM{
				ID:          id,
				Hypervisor:  b.Typ,
				Config:      types.VMConfig{Config: types.Config{CPU: cpu, Memory: mem, Storage: storage}},
				FirstBooted: firstBooted,
			},
			// Real dirs by default: sequences write markers into RunDir, and an empty path would land them in the package dir.
			RunDir: t.TempDir(),
			LogDir: t.TempDir(),
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func seedRunningVM(t *testing.T, b *Backend, id string, cpu int, mem, storage int64) {
	t.Helper()
	seedVMRecord(t, b, id, cpu, mem, storage, true)
	if err := b.dbUpdate(t.Context(), func(idx *VMIndex) error {
		now := time.Now()
		idx.VMs[id].State = types.VMStateRunning
		idx.VMs[id].StartedAt = &now
		return nil
	}); err != nil {
		t.Fatalf("set running: %v", err)
	}
}
