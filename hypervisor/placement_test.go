package hypervisor

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/types"
)

func TestPlaceRecord(t *testing.T) {
	type other struct {
		state     types.VMState
		cpuset    string
		queueCPUs string
	}
	tests := []struct {
		name          string
		cfg           types.Config
		noQueuePins   bool
		selfState     types.VMState
		selfQueueCPUs string
		others        []other
		wantCPUSet    string
		wantQueueCPUs string
	}{
		{name: "explicit cpuset is copied to both", cfg: types.Config{CPU: 4, CPUSetCPUs: "2-3"}, wantCPUSet: "2-3", wantQueueCPUs: "2-3"},
		{name: "a backend without queue pins copies only the cpuset", cfg: types.Config{CPU: 4, CPUSetCPUs: "2-3"}, noQueuePins: true, wantCPUSet: "2-3"},
		{name: "a backend without queue pins still places auto", cfg: types.Config{CPU: 4, CPUSetCPUs: "auto"}, noQueuePins: true, wantCPUSet: "0-7"},
		{name: "a backend without queue pins records nothing by default", cfg: types.Config{CPU: 4}, noQueuePins: true},
		{name: "no cpuset pins one thread per queue on an empty host", cfg: types.Config{CPU: 4}, wantQueueCPUs: "0-3"},
		{name: "live pins push the next VM onto idle cpus", cfg: types.Config{CPU: 4}, others: []other{{state: types.VMStateRunning, queueCPUs: "0-3"}, {state: types.VMStateCreating, queueCPUs: "4-5"}}, wantQueueCPUs: "0-1,6-7"},
		{name: "a placed record counts before its state flips to running", cfg: types.Config{CPU: 4}, others: []other{{state: types.VMStateStopped, queueCPUs: "0-3"}}, wantQueueCPUs: "4-7"},
		{name: "a cpuset without queue pins counts its cpuset", cfg: types.Config{CPU: 4}, others: []other{{state: types.VMStateRunning, cpuset: "0-3"}}, wantQueueCPUs: "4-7"},
		{name: "a VM's own stale pins are not load", cfg: types.Config{CPU: 4}, selfState: types.VMStateRunning, selfQueueCPUs: "0-3", wantQueueCPUs: "0-3"},
		{name: "auto places the cache domain and pins inside it", cfg: types.Config{CPU: 2, CPUSetCPUs: "auto"}, wantCPUSet: "0-7", wantQueueCPUs: "0-1"},
		{name: "a single queue is never pinned", cfg: types.Config{CPU: 1}},
		{name: "auto with a single queue still places the domain", cfg: types.Config{CPU: 1, CPUSetCPUs: "auto"}, wantCPUSet: "0-7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newMeteringTestBackend(t)
			b.PinsQueues = !tt.noQueuePins
			ctx := t.Context()
			if err := b.update(ctx, func(tx *vmTx) error {
				if err := tx.Put("self", &VMRecord{ID: "self", State: tt.selfState, QueueCPUs: tt.selfQueueCPUs}); err != nil {
					return err
				}
				for i, o := range tt.others {
					id := string(rune('a' + i))
					if err := tx.Put(id, &VMRecord{ID: id, State: o.state, CPUSet: o.cpuset, QueueCPUs: o.queueCPUs}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			placed, err := b.placeRecord(ctx, "self", &tt.cfg)
			if err != nil {
				t.Fatalf("placeRecord: %v", err)
			}
			if placed.CPUSet != tt.wantCPUSet || placed.QueueCPUs != tt.wantQueueCPUs {
				t.Errorf("got cpuset %q queue_cpus %q, want %q %q", placed.CPUSet, placed.QueueCPUs, tt.wantCPUSet, tt.wantQueueCPUs)
			}
			stored, err := b.PeekRecord(ctx, "self")
			if err != nil {
				t.Fatalf("PeekRecord: %v", err)
			}
			if stored.CPUSet != placed.CPUSet || stored.QueueCPUs != placed.QueueCPUs {
				t.Errorf("stored cpuset %q queue_cpus %q differ from placed", stored.CPUSet, stored.QueueCPUs)
			}
		})
	}
}

func TestNewBackendPeerNamespaces(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct{ typ, peer string }{{"cloud-hypervisor", "firecracker"}, {"firecracker", "cloud-hypervisor"}} {
		b, err := NewBackend(tt.typ, stubBackendConfig{rootDir: dir}, nil, testNamespace(t, tt.typ, t.TempDir()))
		if err != nil {
			t.Fatalf("NewBackend %s: %v", tt.typ, err)
		}
		if want := []string{VMNamespaceName(tt.peer)}; !slices.Equal(b.PeerNS, want) {
			t.Errorf("%s: PeerNS = %v, want %v", tt.typ, b.PeerNS, want)
		}
	}
}

func TestPlaceRecordCountsPeerNamespaces(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	own, peer := VMNamespaceName("own"), VMNamespaceName("peer")
	store, err := metajson.Open(
		metajson.Namespace{Name: own, FilePath: filepath.Join(dir, "own.json"), LockPath: filepath.Join(dir, "own.lock"), Codec: testVMTables},
		metajson.Namespace{Name: peer, FilePath: filepath.Join(dir, "peer.json"), LockPath: filepath.Join(dir, "peer.lock"), Codec: testVMTables},
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	b := &Backend{Typ: "own", NS: own, PeerNS: []string{peer}, Conf: stubBackendConfig{rootDir: dir}, Meta: store, PinsQueues: true}
	peerBackend := &Backend{Typ: "peer", NS: peer, Meta: store}
	if err := peerBackend.update(ctx, func(tx *vmTx) error {
		return tx.Put("p", &VMRecord{ID: "p", State: types.VMStateRunning, CPUSet: "0-3"})
	}); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	if err := b.update(ctx, func(tx *vmTx) error {
		return tx.Put("self", &VMRecord{ID: "self"})
	}); err != nil {
		t.Fatalf("seed self: %v", err)
	}
	placed, err := b.placeRecord(ctx, "self", &types.Config{CPU: 4})
	if err != nil {
		t.Fatalf("placeRecord: %v", err)
	}
	if placed.QueueCPUs != "4-7" {
		t.Errorf("queue_cpus = %q, want 4-7 clear of the peer backend's cpuset", placed.QueueCPUs)
	}
}

func TestPlacementRowFollowsTheRecord(t *testing.T) {
	for engine, open := range map[string]func(*testing.T, string, string) meta.Store{"json": testNamespace, "sqlite": testSQLiteNamespace} {
		t.Run(engine, func(t *testing.T) {
			b := &Backend{NS: VMNamespaceName("test-hv"), Meta: open(t, "test-hv", t.TempDir())}
			ctx := t.Context()
			row := func() string {
				var got string
				if err := b.view(ctx, func(tx *vmTx) error {
					return tx.placements.Scan(ctx, tx.r, func(id string, cpus *string) error {
						got += id + "=" + *cpus + ";"
						return nil
					})
				}); err != nil {
					t.Fatalf("scan placements: %v", err)
				}
				return got
			}
			put := func(rec *VMRecord) {
				t.Helper()
				if err := b.update(ctx, func(tx *vmTx) error { return tx.Put(rec.ID, rec) }); err != nil {
					t.Fatalf("put: %v", err)
				}
			}
			put(&VMRecord{ID: "a", State: types.VMStateRunning, QueueCPUs: "0-3"})
			put(&VMRecord{ID: "f", State: types.VMStateRunning, CPUSet: "4-7"})
			put(&VMRecord{ID: "n", State: types.VMStateRunning})
			if got := row(); got != "a=0-3;f=4-7;" {
				t.Errorf("rows after placements = %q, want a=0-3;f=4-7;", got)
			}
			put(&VMRecord{ID: "a", State: types.VMStateStopped})
			if err := b.update(ctx, func(tx *vmTx) error { return tx.Del("f") }); err != nil {
				t.Fatalf("del: %v", err)
			}
			if got := row(); got != "" {
				t.Errorf("rows after a cleared placement and a delete = %q, want none", got)
			}
		})
	}
}

func TestPlaceRecordConcurrentLaunchesNeverShareACPU(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	b.PinsQueues = true
	ctx := t.Context()
	ids := []string{"a", "b", "c", "d"}
	if err := b.update(ctx, func(tx *vmTx) error {
		for _, id := range ids {
			if err := tx.Put(id, &VMRecord{ID: id, State: types.VMStateStopped}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		pins []int
	)
	for _, id := range ids {
		wg.Go(func() {
			placed, err := b.placeRecord(ctx, id, &types.Config{CPU: 2})
			if err != nil {
				t.Errorf("placeRecord %s: %v", id, err)
				return
			}
			cpus, _ := cgroup.ParseCPUList(placed.QueueCPUs)
			mu.Lock()
			pins = append(pins, cpus...)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(pins)
	if want := []int{0, 1, 2, 3, 4, 5, 6, 7}; !slices.Equal(pins, want) {
		t.Errorf("pins across four concurrent launches = %v, want every cpu exactly once", pins)
	}
}

func TestLeavingRunningClearsPlacement(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seed := func(id string, state types.VMState) {
		t.Helper()
		if err := b.update(ctx, func(tx *vmTx) error {
			return tx.Put(id, &VMRecord{ID: id, State: state, CPUSet: "0-3", QueueCPUs: "0-3"})
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("stopped", types.VMStateRunning)
	if err := b.UpdateStates(ctx, []string{"stopped"}, types.VMStateStopped); err != nil {
		t.Fatalf("UpdateStates: %v", err)
	}
	seed("failed", types.VMStateStopped)
	b.markFailedOperation(ctx, "failed", false)
	for _, id := range []string{"stopped", "failed"} {
		rec, err := b.PeekRecord(ctx, id)
		if err != nil {
			t.Fatalf("PeekRecord %s: %v", id, err)
		}
		if rec.CPUSet != "" || rec.QueueCPUs != "" {
			t.Errorf("%s: cpuset %q queue_cpus %q survived leaving running", id, rec.CPUSet, rec.QueueCPUs)
		}
	}
}
