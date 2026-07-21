package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type convergeCall struct {
	vmID string
	gen  uint64
}

// fakeSupervisor scripts one backend's record set, liveness and lock state.
type fakeSupervisor struct {
	mu         sync.Mutex
	records    map[string]*hypervisor.VMRecord
	tombstoned map[string]struct{}
	live       map[string]utils.ProcRef
	busy       map[string]struct{}
	lockErr    error
	observeErr error

	converged []convergeCall
	adopted   []string
	collected []string
	quiesced  []string
}

func newFake() *fakeSupervisor {
	return &fakeSupervisor{
		records:    map[string]*hypervisor.VMRecord{},
		tombstoned: map[string]struct{}{},
		live:       map[string]utils.ProcRef{},
		busy:       map[string]struct{}{},
	}
}

func (f *fakeSupervisor) put(rec *hypervisor.VMRecord) *fakeSupervisor {
	f.records[rec.ID] = rec
	return f
}

func (f *fakeSupervisor) Type() string { return "fake-hv" }

func (f *fakeSupervisor) ScanSupervision(context.Context) (hypervisor.SupervisionScan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scan := hypervisor.SupervisionScan{Tombstoned: f.tombstoned}
	for _, rec := range f.records {
		scan.Records = append(scan.Records, rec)
	}
	return scan, nil
}

func (f *fakeSupervisor) ObserveVMM(_ context.Context, rec *hypervisor.VMRecord) (utils.ProcRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observeErr != nil {
		return utils.ProcRef{}, f.observeErr
	}
	proc, ok := f.live[rec.ID]
	if !ok {
		return utils.ProcRef{}, hypervisor.ErrNotRunning
	}
	return proc, nil
}

func (f *fakeSupervisor) TryLockVMOps(_ context.Context, vmID string) (func(), bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lockErr != nil {
		return nil, false, f.lockErr
	}
	if _, held := f.busy[vmID]; held {
		return nil, false, nil
	}
	return func() {}, true, nil
}

func (f *fakeSupervisor) PeekRecord(_ context.Context, vmID string) (*hypervisor.VMRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[vmID], nil
}

func (f *fakeSupervisor) ConvergeDead(_ context.Context, vmID string, gen uint64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.converged = append(f.converged, convergeCall{vmID: vmID, gen: gen})
	if rec := f.records[vmID]; rec != nil {
		rec.State = types.VMStateStopped
		rec.TransitionGeneration++
	}
	return nil
}

func (f *fakeSupervisor) QuiesceIfPending(_ context.Context, vmID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quiesced = append(f.quiesced, vmID)
	if rec := f.records[vmID]; rec != nil {
		rec.QuiescePending = false
	}
	return nil
}

func (f *fakeSupervisor) ReconcileToRunning(_ context.Context, vmID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopted = append(f.adopted, vmID)
	if rec := f.records[vmID]; rec != nil {
		rec.State = types.VMStateRunning
		rec.TransitionGeneration++
	}
}

func (f *fakeSupervisor) CollectStaleCreate(_ context.Context, vmID string, _ *hypervisor.VMRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collected = append(f.collected, vmID)
	delete(f.records, vmID)
	return nil
}

func newTestDaemon(t *testing.T, f *fakeSupervisor) *Daemon {
	t.Helper()
	d, err := New(Config{RootDir: t.TempDir()}, nil, []Supervisor{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func runningRec(id string, gen uint64) *hypervisor.VMRecord {
	return &hypervisor.VMRecord{VM: types.VM{ID: id, State: types.VMStateRunning, TransitionGeneration: gen}}
}

func TestReconcileConvergesDeadRunningRecord(t *testing.T) {
	f := newFake().put(runningRec("vm1", 7))
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.converged) != 1 || f.converged[0] != (convergeCall{vmID: "vm1", gen: 7}) {
		t.Fatalf("got %+v, want one convergence fenced on generation 7", f.converged)
	}
}

func TestReconcileSkipsVMWithBusyOpsLock(t *testing.T) {
	f := newFake().put(runningRec("vm1", 1))
	f.busy["vm1"] = struct{}{}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.converged) != 0 {
		t.Errorf("got %+v, want no convergence while an operation owns the VM", f.converged)
	}
}

func TestReconcileSkipsOnLockError(t *testing.T) {
	f := newFake().put(runningRec("vm1", 1))
	f.lockErr = fmt.Errorf("no such directory")
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.converged) != 0 {
		t.Errorf("got %+v, want no convergence when the lock could not be evaluated", f.converged)
	}
}

// An inconclusive liveness probe must never be read as an exit.
func TestReconcileSkipsInconclusiveLiveness(t *testing.T) {
	f := newFake().put(runningRec("vm1", 1))
	f.observeErr = fmt.Errorf("/proc scan errored")
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.converged) != 0 {
		t.Errorf("got %+v, want no convergence on an inconclusive probe", f.converged)
	}
}

func TestReconcileSkipsTombstonedVM(t *testing.T) {
	f := newFake().put(runningRec("vm1", 1))
	f.tombstoned["vm1"] = struct{}{}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.converged) != 0 {
		t.Errorf("got %+v, want an unfinished delete left to the entry guard and GC", f.converged)
	}
}

func TestReconcileAdoptsLiveDriftedRecord(t *testing.T) {
	rec := &hypervisor.VMRecord{VM: types.VM{ID: "vm1", State: types.VMStateStopped, TransitionGeneration: 3}}
	f := newFake().put(rec)
	f.live["vm1"] = utils.ProcRef{PID: 4242, Start: 99}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.adopted) != 1 || f.adopted[0] != "vm1" {
		t.Fatalf("got adopted %v, want the live VM repaired to running", f.adopted)
	}
	if len(f.converged) != 0 {
		t.Errorf("got %+v, want no convergence for a live VM", f.converged)
	}
}

func TestReconcileLeavesQuarantinedRecordAlone(t *testing.T) {
	rec := &hypervisor.VMRecord{VM: types.VM{ID: "vm1", State: types.VMStateError}, Quarantine: "partial restore"}
	f := newFake().put(rec)
	f.live["vm1"] = utils.ProcRef{PID: 4242, Start: 99}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.adopted) != 0 {
		t.Errorf("got adopted %v, want a quarantined record left for restore or rm", f.adopted)
	}
}

func TestReconcileCollectsOwnerlessCreating(t *testing.T) {
	f := newFake().put(&hypervisor.VMRecord{VM: types.VM{ID: "vm1", State: types.VMStateCreating}})
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.collected) != 1 || f.collected[0] != "vm1" {
		t.Fatalf("got collected %v, want the ownerless placeholder reclaimed", f.collected)
	}
}

// A held ops lock is the create owner still working; the placeholder is not ownerless.
func TestReconcileLeavesInFlightCreating(t *testing.T) {
	f := newFake().put(&hypervisor.VMRecord{VM: types.VM{ID: "vm1", State: types.VMStateCreating}})
	f.busy["vm1"] = struct{}{}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.collected) != 0 {
		t.Errorf("got collected %v, want an in-flight create untouched", f.collected)
	}
}

func TestReconcileRetriesPendingQuiesce(t *testing.T) {
	rec := &hypervisor.VMRecord{VM: types.VM{ID: "vm1", State: types.VMStateStopped, TransitionGeneration: 2}}
	rec.QuiescePending = true
	f := newFake().put(rec)
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.quiesced) != 1 || f.quiesced[0] != "vm1" {
		t.Fatalf("got quiesced %v, want the pending quiesce retried", f.quiesced)
	}
	if len(f.converged) != 0 {
		t.Errorf("got %+v, want no second transition for an already-stopped VM", f.converged)
	}
}

func TestReconcileLeavesSettledVMAlone(t *testing.T) {
	f := newFake().put(&hypervisor.VMRecord{VM: types.VM{ID: "vm1", State: types.VMStateStopped, TransitionGeneration: 2}})
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.converged)+len(f.quiesced)+len(f.adopted) != 0 {
		t.Error("a settled record was rewritten")
	}
}

func TestHandleExitConvergesWatchedGeneration(t *testing.T) {
	f := newFake().put(runningRec("vm1", 4))
	d := newTestDaemon(t, f)
	at := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	d.handleExit(t.Context(), exitEvent{key: watchKey{backend: f.Type(), vmID: "vm1"}, gen: 4, at: at})

	if len(f.converged) != 1 || f.converged[0].gen != 4 {
		t.Fatalf("got %+v, want one convergence at generation 4", f.converged)
	}
}

// An exit observed before a restart must not date or re-label the newer transition.
func TestHandleExitDropsStaleGeneration(t *testing.T) {
	f := newFake().put(runningRec("vm1", 9))
	d := newTestDaemon(t, f)

	d.handleExit(t.Context(), exitEvent{key: watchKey{backend: f.Type(), vmID: "vm1"}, gen: 4, at: time.Now()})

	if len(f.converged) != 0 {
		t.Errorf("got %+v, want the stale event dropped", f.converged)
	}
}

func TestHandleExitSkipsRevivedVM(t *testing.T) {
	f := newFake().put(runningRec("vm1", 4))
	f.live["vm1"] = utils.ProcRef{PID: 4242, Start: 7}
	d := newTestDaemon(t, f)

	d.handleExit(t.Context(), exitEvent{key: watchKey{backend: f.Type(), vmID: "vm1"}, gen: 4, at: time.Now()})

	if len(f.converged) != 0 {
		t.Errorf("got %+v, want no convergence while a VMM generation is live", f.converged)
	}
}

func TestHandleExitIgnoresDeletedRecord(t *testing.T) {
	f := newFake()
	d := newTestDaemon(t, f)

	d.handleExit(t.Context(), exitEvent{key: watchKey{backend: f.Type(), vmID: "gone"}, gen: 1, at: time.Now()})

	if len(f.converged) != 0 {
		t.Errorf("got %+v, want nothing for a record removed while the event queued", f.converged)
	}
}
