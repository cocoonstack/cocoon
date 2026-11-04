package daemon

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/meta"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestReconcileConvergesDeadRunningRecord(t *testing.T) {
	f := newFake().put(runningRec("vm1", 7))
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.converged) != 1 || f.converged[0].vmID != "vm1" || f.converged[0].gen != 7 {
		t.Fatalf("got %+v, want one convergence fenced on generation 7", f.converged)
	}
}

func TestReconcileScanFailureKeepsPublishedStatuses(t *testing.T) {
	f := newFake().put(runningRec("vm1", 1))
	d := newTestDaemon(t, f)
	d.reconcile(t.Context())

	f.scanErr = errors.New("store unreadable")
	d.reconcile(t.Context())

	statuses, h := d.state.snapshot()
	if h.ok {
		t.Error("health ok after a scan failure")
	}
	if len(statuses) != 1 || statuses[0].ID != "vm1" {
		t.Fatalf("got %+v, want the previous pass's vm1 carried forward", statuses)
	}
}

func TestReconcileResumesInterruptedDelete(t *testing.T) {
	f := newFake().put(runningRec("vm1", 1))
	f.tombstoned["vm1"] = struct{}{}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.converged) != 0 {
		t.Errorf("got %+v, want no state convergence for a record being deleted", f.converged)
	}
	if len(f.resumed) != 1 || f.resumed[0] != "vm1" {
		t.Fatalf("got resumed %v, want the interrupted delete finished", f.resumed)
	}
}

func TestReconcileLeavesInterruptedDeleteLockedByAnOwner(t *testing.T) {
	f := newFake().put(runningRec("vm1", 1))
	f.tombstoned["vm1"] = struct{}{}
	f.busy["vm1"] = struct{}{}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.resumed) != 0 {
		t.Errorf("got resumed %v, want the owning operation left to finish it", f.resumed)
	}
}

func TestReconcileAdoptsLiveDriftedRecord(t *testing.T) {
	rec := &hypervisor.VMRecord{ID: "vm1", State: types.VMStateStopped, TransitionGeneration: 3}
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
	rec := &hypervisor.VMRecord{ID: "vm1", State: types.VMStateError, Quarantine: "partial restore"}
	f := newFake().put(rec)
	f.live["vm1"] = utils.ProcRef{PID: 4242, Start: 99}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.adopted) != 0 {
		t.Errorf("got adopted %v, want a quarantined record left for restore or rm", f.adopted)
	}
}

func TestReconcileCollectsOwnerlessCreating(t *testing.T) {
	f := newFake().put(&hypervisor.VMRecord{ID: "vm1", State: types.VMStateCreating})
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.collected) != 1 || f.collected[0] != "vm1" {
		t.Fatalf("got collected %v, want the ownerless placeholder reclaimed", f.collected)
	}
}

func TestReconcileLeavesInFlightCreating(t *testing.T) {
	f := newFake().put(&hypervisor.VMRecord{ID: "vm1", State: types.VMStateCreating})
	f.busy["vm1"] = struct{}{}
	newTestDaemon(t, f).reconcile(t.Context())

	if len(f.collected) != 0 {
		t.Errorf("got collected %v, want an in-flight create untouched", f.collected)
	}
}

func TestReconcileRetriesPendingQuiesce(t *testing.T) {
	rec := &hypervisor.VMRecord{ID: "vm1", State: types.VMStateStopped, TransitionGeneration: 2}
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
	f := newFake().put(&hypervisor.VMRecord{ID: "vm1", State: types.VMStateStopped, TransitionGeneration: 2})
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
	if !f.converged[0].at.Equal(at) {
		t.Errorf("got stop dated %v, want the watcher's observation %v", f.converged[0].at, at)
	}
}

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

func TestFailingRollForwardDoesNotWakeItsOwnDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	const ns = "vms_fake"
	decl := metasqlite.Namespace{Name: ns, Tables: []string{"records", tombstone.TableName}}
	dir := t.TempDir()
	db := filepath.Join(dir, "meta", metasqlite.DBFileName)
	if err := metasqlite.Init(ctx, db, decl); err != nil {
		t.Fatal(err)
	}
	store, err := metasqlite.Open(db, decl)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ts := tombstone.NewTable(ns)
	if err := store.Update(ctx, meta.Scope{Write: ns}, meta.CommitDurable, func(w meta.Writer) error {
		lease, err := ts.Lease(ctx, w, "vm1", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: []byte(`{}`)})
		if err != nil {
			return err
		}
		return ts.MarkDeleting(ctx, w, "vm1", lease)
	}); err != nil {
		t.Fatal(err)
	}
	f := &committingSupervisor{fakeSupervisor: newFake().put(runningRec("vm1", 1)), store: store, ns: ns}
	f.tombstoned["vm1"] = struct{}{}
	if err := New(Config{RootDir: dir, ReconcileInterval: time.Hour}, store, []Supervisor{f}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.passes.Load(); got != 1 {
		t.Fatalf("passes = %d with an hourly ticker, want 1", got)
	}
}

func TestPassCountsDegradedVMsWithoutGoingUnready(t *testing.T) {
	tests := []struct {
		name      string
		arrange   func(*fakeSupervisor)
		degraded  int
		converged int
	}{
		{"clean pass", func(*fakeSupervisor) {}, 0, 1},
		{"busy ops lock", func(f *fakeSupervisor) { f.busy["vm1"] = struct{}{} }, 0, 0},
		{"ops lock error", func(f *fakeSupervisor) { f.lockErr = errors.New("no such directory") }, 1, 0},
		{"inconclusive liveness", func(f *fakeSupervisor) { f.observeErr = errors.New("/proc scan errored") }, 1, 0},
		{"delete cannot finish", func(f *fakeSupervisor) {
			f.tombstoned["vm1"] = struct{}{}
			f.resumeErr = errors.New("cni down")
		}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake().put(runningRec("vm1", 1))
			tt.arrange(f)
			d := newTestDaemon(t, f)
			d.reconcile(t.Context())

			_, h := d.state.snapshot()
			if h.degraded != tt.degraded {
				t.Errorf("got degraded=%d, want %d", h.degraded, tt.degraded)
			}
			if !h.ok {
				t.Error("a pass that ran reported unready; only a scan failure may")
			}
			if len(f.converged) != tt.converged {
				t.Errorf("got %+v, want %d convergences", f.converged, tt.converged)
			}
		})
	}
}

type convergeCall struct {
	vmID string
	gen  uint64
	at   time.Time
}

type fakeSupervisor struct {
	mu         sync.Mutex
	records    map[string]*hypervisor.VMRecord
	tombstoned map[string]struct{}
	live       map[string]utils.ProcRef
	busy       map[string]struct{}
	lockErr    error
	observeErr error

	resumeErr error
	scanErr   error

	converged []convergeCall
	adopted   []string
	collected []string
	quiesced  []string
	resumed   []string
}

func (f *fakeSupervisor) Type() string { return "fake-hv" }

func (f *fakeSupervisor) ScanSupervision(context.Context) (hypervisor.SupervisionScan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scanErr != nil {
		return hypervisor.SupervisionScan{}, f.scanErr
	}
	return hypervisor.SupervisionScan{Tombstoned: f.tombstoned, Records: slices.Collect(maps.Values(f.records))}, nil
}

func (f *fakeSupervisor) ObserveVMMIn(ctx context.Context, rec *hypervisor.VMRecord, _ utils.ProcScan) (utils.ProcRef, error) {
	return f.ObserveVMM(ctx, rec)
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
	busy, err := f.lockState(vmID)
	if err != nil || busy {
		return nil, false, err
	}
	return func() {}, true, nil
}

func (f *fakeSupervisor) PeekRecord(_ context.Context, vmID string) (*hypervisor.VMRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[vmID], nil
}

func (f *fakeSupervisor) ConvergeDead(_ context.Context, vmID string, gen uint64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.records[vmID]
	if rec != nil && hypervisor.NeedsDeadConvergence(rec) && rec.TransitionGeneration == gen {
		f.converged = append(f.converged, convergeCall{vmID: vmID, gen: gen, at: at})
		rec.State = types.VMStateStopped
		rec.TransitionGeneration++
	}
	if rec != nil && rec.QuiescePending {
		f.quiesced = append(f.quiesced, vmID)
		rec.QuiescePending = false
	}
	return nil
}

func (f *fakeSupervisor) ReconcileToRunning(_ context.Context, vmID string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.records[vmID]
	if rec == nil || rec.Quarantine != "" || rec.State == types.VMStateCreating {
		return 0, nil
	}
	f.adopted = append(f.adopted, vmID)
	rec.State = types.VMStateRunning
	rec.TransitionGeneration++
	return rec.TransitionGeneration, nil
}

func (f *fakeSupervisor) RecoverTombstone(_ context.Context, vmID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, vmID)
	if f.resumeErr != nil {
		return false, f.resumeErr
	}
	delete(f.records, vmID)
	delete(f.tombstoned, vmID)
	return true, nil
}

func (f *fakeSupervisor) ReconcileStaleCreate(_ context.Context, vmID string) (hypervisor.StaleCreateOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	busy, err := f.lockState(vmID)
	if err != nil {
		return "", err
	}
	if busy {
		return hypervisor.StaleCreateBusy, nil
	}
	rec := f.records[vmID]
	if rec == nil {
		return hypervisor.StaleCreateNotFound, nil
	}
	if rec.State != types.VMStateCreating {
		return hypervisor.StaleCreateNotCreating, nil
	}
	f.collected = append(f.collected, vmID)
	delete(f.records, vmID)
	return hypervisor.StaleCreateCollected, nil
}

func (f *fakeSupervisor) lockState(vmID string) (busy bool, err error) {
	if f.lockErr != nil {
		return false, f.lockErr
	}
	_, held := f.busy[vmID]
	return held, nil
}

func (f *fakeSupervisor) put(rec *hypervisor.VMRecord) *fakeSupervisor {
	f.records[rec.ID] = rec
	return f
}

func newFake() *fakeSupervisor {
	return &fakeSupervisor{
		records:    map[string]*hypervisor.VMRecord{},
		tombstoned: map[string]struct{}{},
		live:       map[string]utils.ProcRef{},
		busy:       map[string]struct{}{},
	}
}

type committingSupervisor struct {
	*fakeSupervisor
	store  meta.Store
	ns     string
	passes atomic.Int64
}

func (s *committingSupervisor) ScanSupervision(ctx context.Context) (hypervisor.SupervisionScan, error) {
	s.passes.Add(1)
	return s.fakeSupervisor.ScanSupervision(ctx)
}

func (s *committingSupervisor) RecoverTombstone(ctx context.Context, id string) (bool, error) {
	var cl struct{}
	if err := s.store.Update(ctx, meta.Scope{Write: s.ns}, meta.CommitDurable, func(w meta.Writer) error {
		_, err := tombstone.NewTable(s.ns).Recover(ctx, w, id, &cl)
		return err
	}); err != nil {
		return false, err
	}
	return false, errors.New("cni down")
}

func newTestDaemon(t *testing.T, f *fakeSupervisor) *Daemon {
	t.Helper()
	return New(Config{RootDir: t.TempDir()}, nil, []Supervisor{f})
}

func runningRec(id string, gen uint64) *hypervisor.VMRecord {
	return &hypervisor.VMRecord{ID: id, State: types.VMStateRunning, TransitionGeneration: gen}
}
