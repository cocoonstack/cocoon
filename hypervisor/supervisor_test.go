package hypervisor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
)

func TestConvergeDeadMarksUnexpectedExit(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 2, 1<<30, 10<<30)
	observed := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	if err := b.ConvergeDead(ctx, "vm1", recordOf(t, b, "vm1").TransitionGeneration, observed); err != nil {
		t.Fatalf("ConvergeDead: %v", err)
	}

	got := recordOf(t, b, "vm1")
	if got.State != types.VMStateStopped {
		t.Errorf("got state %q, want stopped", got.State)
	}
	if got.LastTransitionReason != types.TransitionUnexpectedExit {
		t.Errorf("got reason %q, want %q", got.LastTransitionReason, types.TransitionUnexpectedExit)
	}
	if got.StoppedAt == nil || !got.StoppedAt.Equal(observed) {
		t.Errorf("got StoppedAt %v, want the observation time %v", got.StoppedAt, observed)
	}
	entries := rec.Entries()
	if len(entries) != 1 || entries[0].Kind != metering.KindVMComputeStop || entries[0].Reason != metering.ReasonStopCrash {
		t.Fatalf("got %+v, want one compute.stop/stop-crash", entries)
	}
	if !entries[0].EmittedAt.Equal(observed) {
		t.Errorf("got EmittedAt %v, want the observation time %v", entries[0].EmittedAt, observed)
	}
}

// A second convergence must not re-close the ledger: the generation it fences on has already advanced.
func TestConvergeDeadIsIdempotent(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 1, 1<<30, 10<<30)
	gen := recordOf(t, b, "vm1").TransitionGeneration

	for range 3 {
		if err := b.ConvergeDead(ctx, "vm1", gen, timeNow()); err != nil {
			t.Fatalf("ConvergeDead: %v", err)
		}
	}
	if got := len(rec.Entries()); got != 1 {
		t.Errorf("got %d metering entries, want 1", got)
	}
	if got := recordOf(t, b, "vm1").TransitionGeneration; got != gen+1 {
		t.Errorf("got generation %d, want exactly one increment from %d", got, gen)
	}
}

func TestConvergeDeadFencesStaleGeneration(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 1, 1<<30, 10<<30)
	gen := recordOf(t, b, "vm1").TransitionGeneration

	if err := b.ConvergeDead(ctx, "vm1", gen+1, timeNow()); err != nil {
		t.Fatalf("ConvergeDead: %v", err)
	}
	if got := recordOf(t, b, "vm1").State; got != types.VMStateRunning {
		t.Errorf("got state %q, want the record untouched at running", got)
	}
	if got := len(rec.Entries()); got != 0 {
		t.Errorf("got %d metering entries, want 0 for a stale observation", got)
	}
}

// A record left Running by an older cocoon carries no open interval; the state must still converge, without an unmatched stop.
func TestConvergeDeadStopsLegacyRunningWithoutInterval(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs["vm1"].State = types.VMStateRunning
		return nil
	}); err != nil {
		t.Fatalf("seed legacy running: %v", err)
	}

	if err := b.ConvergeDead(ctx, "vm1", recordOf(t, b, "vm1").TransitionGeneration, timeNow()); err != nil {
		t.Fatalf("ConvergeDead: %v", err)
	}
	if got := recordOf(t, b, "vm1").State; got != types.VMStateStopped {
		t.Errorf("got state %q, want stopped", got)
	}
	if got := len(rec.Entries()); got != 0 {
		t.Errorf("got %d metering entries, want 0 without an open interval", got)
	}
}

func TestConvergeDeadQuiescesPlumbedVM(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedPlumbedVM(t, b, "vm1")
	var quiesced []string
	b.SetNetwork(stubNetwork{quiesce: func(_ context.Context, vm *types.VM) error {
		quiesced = append(quiesced, vm.ID)
		return nil
	}})

	if err := b.ConvergeDead(ctx, "vm1", recordOf(t, b, "vm1").TransitionGeneration, timeNow()); err != nil {
		t.Fatalf("ConvergeDead: %v", err)
	}
	if len(quiesced) != 1 || quiesced[0] != "vm1" {
		t.Errorf("got quiesce calls %v, want one for vm1", quiesced)
	}
	if recordOf(t, b, "vm1").QuiescePending {
		t.Error("QuiescePending still set after a successful quiesce")
	}
}

func TestConvergeDeadKeepsPendingWhenQuiesceFails(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedPlumbedVM(t, b, "vm1")
	b.SetNetwork(stubNetwork{quiesce: func(context.Context, *types.VM) error {
		return fmt.Errorf("netns gone")
	}})

	if err := b.ConvergeDead(ctx, "vm1", recordOf(t, b, "vm1").TransitionGeneration, timeNow()); err == nil {
		t.Fatal("ConvergeDead returned nil, want the quiesce failure surfaced")
	}
	got := recordOf(t, b, "vm1")
	if !got.QuiescePending {
		t.Error("QuiescePending cleared despite a failed quiesce")
	}
	if got.State != types.VMStateStopped {
		t.Errorf("got state %q, want the transition to have landed anyway", got.State)
	}
}

// The clear is fenced: a transition landing during the quiesce owns the pending flag it set.
func TestQuiesceIfPendingFencedByGeneration(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedPlumbedVM(t, b, "vm1")
	b.SetNetwork(stubNetwork{quiesce: func(context.Context, *types.VM) error {
		// A concurrent stop transitions the record while the quiesce is in flight.
		return b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped)
	}})
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs["vm1"].QuiescePending = true
		return nil
	}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	if err := b.QuiesceIfPending(ctx, "vm1"); err != nil {
		t.Fatalf("QuiesceIfPending: %v", err)
	}
	if !recordOf(t, b, "vm1").QuiescePending {
		t.Error("stale clear won: the newer transition's pending work was dropped")
	}
}

func TestBatchMarkStartedClearsQuiescePending(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedPlumbedVM(t, b, "vm1")
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs["vm1"].QuiescePending = true
		return nil
	}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	if err := b.BatchMarkStarted(ctx, []string{"vm1"}); err != nil {
		t.Fatalf("BatchMarkStarted: %v", err)
	}
	if recordOf(t, b, "vm1").QuiescePending {
		t.Error("a start left stale pending quiesce work that would down its own plumbing")
	}
}

func TestUpdateStatesStoppedSchedulesQuiesce(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedPlumbedVM(t, b, "vm1")

	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates: %v", err)
	}
	got := recordOf(t, b, "vm1")
	if !got.QuiescePending {
		t.Error("a stop of a plumbed VM scheduled no quiesce")
	}
	if got.LastTransitionReason != types.TransitionStopUser {
		t.Errorf("got reason %q, want %q", got.LastTransitionReason, types.TransitionStopUser)
	}
}

func TestUpdateStatesStoppedSkipsQuiesceWithoutPlumbing(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedRunningVM(t, b, "vm1", 1, 1<<30, 10<<30)

	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates: %v", err)
	}
	if recordOf(t, b, "vm1").QuiescePending {
		t.Error("scheduled a quiesce for a VM with no host plumbing")
	}
}

func TestNeedsDeadConvergence(t *testing.T) {
	started := time.Now()
	tests := []struct {
		name string
		rec  *VMRecord
		want bool
	}{
		{"nil", nil, false},
		{"creating stays with the create path", &VMRecord{VM: types.VM{State: types.VMStateCreating}}, false},
		{"running", &VMRecord{VM: types.VM{State: types.VMStateRunning}}, true},
		{"stopped and settled", &VMRecord{VM: types.VM{State: types.VMStateStopped}}, false},
		{"error with an open interval", &VMRecord{VM: types.VM{State: types.VMStateError, StartedAt: &started}}, true},
		{"error already closed", &VMRecord{VM: types.VM{State: types.VMStateError, StartedAt: &started, StoppedAt: &started}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeedsDeadConvergence(tt.rec); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCollectStaleCreateFreesTheName(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	cfg := &types.VMConfig{Name: "alpha", Config: types.Config{CPU: 1}}
	if err := b.ReserveVM(ctx, "vm1", cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("ReserveVM: %v", err)
	}

	if err := b.CollectStaleCreate(ctx, "vm1", recordOf(t, b, "vm1")); err != nil {
		t.Fatalf("CollectStaleCreate: %v", err)
	}
	if rec, err := b.PeekRecord(ctx, "vm1"); err != nil || rec != nil {
		t.Errorf("got record %v (err %v), want it collected", rec, err)
	}
	if _, err := b.ResolveRef(ctx, "alpha"); err == nil {
		t.Error("the name is still reserved; a retry would be blocked until GC")
	}
}

func TestTryLockVMOpsReportsBusyWithoutError(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()

	held, err := b.LockVMOps(ctx, "vm1")
	if err != nil {
		t.Fatalf("LockVMOps: %v", err)
	}
	defer held()

	unlock, ok, err := b.TryLockVMOps(ctx, "vm1")
	if err != nil {
		t.Fatalf("got error %v, want a plain busy report", err)
	}
	if ok {
		unlock()
		t.Fatal("acquired an ops lock already held by an in-flight operation")
	}
}

// StoppedAt is when the VM was observed stopped, so a record predating the
// interval bookkeeping still gets one.
func TestConvergeDeadDatesARecordWithNoOpenInterval(t *testing.T) {
	b, rec := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs["vm1"].State = types.VMStateRunning
		return nil
	}); err != nil {
		t.Fatalf("seed legacy running: %v", err)
	}
	observed := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)

	if err := b.ConvergeDead(ctx, "vm1", recordOf(t, b, "vm1").TransitionGeneration, observed); err != nil {
		t.Fatalf("ConvergeDead: %v", err)
	}
	got := recordOf(t, b, "vm1")
	if got.StoppedAt == nil || !got.StoppedAt.Equal(observed) {
		t.Errorf("got StoppedAt %v, want the observation time %v", got.StoppedAt, observed)
	}
	if n := len(rec.Entries()); n != 0 {
		t.Errorf("got %d metering entries, want 0 without an open interval", n)
	}
}

func TestUpdateStatesDatesAStoppedRecordThatWasRunning(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs["vm1"].State = types.VMStateRunning
		return nil
	}); err != nil {
		t.Fatalf("seed legacy running: %v", err)
	}

	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates: %v", err)
	}
	if recordOf(t, b, "vm1").StoppedAt == nil {
		t.Error("a running record stopped by the user got no stop time")
	}
}

// A VM that never started must not be dated as though it had.
func TestUpdateStatesLeavesACreatedRecordUndated(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, false)

	if err := b.UpdateStates(ctx, []string{"vm1"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates: %v", err)
	}
	if got := recordOf(t, b, "vm1"); got.StoppedAt != nil {
		t.Errorf("got StoppedAt %v for a VM that never ran, want nil", got.StoppedAt)
	}
}

// seedPlumbedVM gives a running VM the persisted network state a stop must quiesce.
func seedPlumbedVM(t *testing.T, b *Backend, id string) {
	t.Helper()
	seedRunningVM(t, b, id, 1, 1<<30, 10<<30)
	if err := b.dbUpdate(t.Context(), func(idx *VMIndex) error {
		idx.VMs[id].NetSetup = types.NetSetup{
			NetBackend:     types.BackendCNI,
			NetworkConfigs: []*types.NetworkConfig{{}},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed network: %v", err)
	}
}

func recordOf(t *testing.T, b *Backend, id string) *VMRecord {
	t.Helper()
	rec, err := b.PeekRecord(t.Context(), id)
	if err != nil {
		t.Fatalf("peek %s: %v", id, err)
	}
	if rec == nil {
		t.Fatalf("record %s is gone", id)
	}
	return rec
}
