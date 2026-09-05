package cloudhypervisor

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/hypervisor"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

func TestReconcileOrphanNICs(t *testing.T) {
	hc, removed := newCHStubClient(t, []chNet{
		{ID: "cocoon-net-aabbccddee01", MAC: "aa:bb:cc:dd:ee:01", TAP: "tapvm1beef-0"},
		{ID: "cocoon-net-aabbccddee02", MAC: "aa:bb:cc:dd:ee:02", TAP: "tapvm1beef-1"},
		{ID: "_net0", MAC: "aa:bb:cc:dd:ee:99", TAP: "tapvm1beef-9"},
	})
	plumbing := &stubPlumbing{}

	info, err := getVMInfo(t.Context(), hc)
	if err != nil {
		t.Fatalf("vm.info: %v", err)
	}
	recorded := []*types.NetworkConfig{{MAC: "AA:BB:CC:DD:EE:01"}}
	if err := reconcileOrphanNICs(t.Context(), hc, info, "vm1", recorded, plumbing); err != nil {
		t.Fatalf("reconcileOrphanNICs: %v", err)
	}
	if got := removed(); len(got) != 1 || got[0] != "cocoon-net-aabbccddee02" {
		t.Fatalf("removed = %v, want only the unrecorded cocoon NIC", got)
	}
	if len(plumbing.removed) != 1 || plumbing.removed[0] != 1 {
		t.Fatalf("plumbing.removed = %v, want the orphan's host slot 1 reclaimed", plumbing.removed)
	}
}

func TestReconcileOrphanNICsReclaimsSlotOnEjectTimeout(t *testing.T) {
	hc, removed := newCHStubClient(t, []chNet{
		{ID: "cocoon-net-aabbccddee02", MAC: "aa:bb:cc:dd:ee:02", TAP: "tapvm1beef-1"},
	}, "cocoon-net-aabbccddee02")
	plumbing := &stubPlumbing{}

	info, err := getVMInfo(t.Context(), hc)
	if err != nil {
		t.Fatalf("vm.info: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err = reconcileOrphanNICs(ctx, hc, info, "vm1", nil, plumbing)
	if err == nil {
		t.Fatal("a timed-out eject wait must surface an error")
	}
	if got := removed(); len(got) != 1 || got[0] != "cocoon-net-aabbccddee02" {
		t.Fatalf("removed = %v, want the orphan ejected", got)
	}
	if len(plumbing.removed) != 1 || plumbing.removed[0] != 1 {
		t.Fatalf("plumbing.removed = %v, want host slot 1 reclaimed despite the timeout", plumbing.removed)
	}
}

func TestResolveFailedPersist(t *testing.T) {
	ch := newTestCH(t)
	ctx := t.Context()
	nc := &types.NetworkConfig{MAC: "aa:bb:cc:dd:ee:01", TAP: "tap-vm1-0"}
	hc, removed := newCHStubClient(t, []chNet{{ID: "cocoon-net-aabbccddee01", MAC: nc.MAC}})
	plumbing := &stubPlumbing{}

	seedNetVM(t, ch, "vm1", nc)
	committed, err := ch.resolveFailedPersist(ctx, hc, plumbing, "vm1", nc, "cocoon-net-aabbccddee01", 0)
	if err != nil || !committed {
		t.Fatalf("committed write must keep the device: committed=%v err=%v", committed, err)
	}
	if len(removed()) != 0 || len(plumbing.removed) != 0 {
		t.Fatal("no teardown may run when the record carries the NIC")
	}

	if err := ch.UpdateRecord(ctx, "vm1", func(r *hypervisor.VMRecord) error {
		r.NetworkConfigs = nil
		return nil
	}); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	committed, err = ch.resolveFailedPersist(ctx, hc, plumbing, "vm1", nc, "cocoon-net-aabbccddee01", 0)
	if err != nil || committed {
		t.Fatalf("conclusive miss must report uncommitted: committed=%v err=%v", committed, err)
	}
	if got := removed(); len(got) != 1 || got[0] != "cocoon-net-aabbccddee01" {
		t.Fatalf("removed = %v, want the half-added device ejected", got)
	}
	if len(plumbing.removed) != 1 || plumbing.removed[0] != 0 {
		t.Fatalf("plumbing.removed = %v, want nic 0 torn down", plumbing.removed)
	}
}

func TestNetResizeRemoveResumesWithoutLiveDevice(t *testing.T) {
	ch := newTestCH(t)
	ctx := t.Context()
	nc := &types.NetworkConfig{MAC: "aa:bb:cc:dd:ee:07", TAP: "tap-vm7-0"}
	seedNetVM(t, ch, "vm7", nc)

	hc, _ := newCHStubClient(t, nil)
	plumbing := &stubPlumbing{}

	res, err := ch.netResizeRemove(ctx, hc, &chVMInfoResponse{}, "vm7", []*types.NetworkConfig{nc}, plumbing, 1, 0, netresize.Result{Before: 1, After: 1})
	if err != nil {
		t.Fatalf("resume remove must not error on a missing live device: %v", err)
	}
	if res.After != 0 || len(res.Removed) != 1 {
		t.Fatalf("res = %+v, want the NIC removed and After=0", res)
	}
	rec, err := ch.PeekRecord(ctx, "vm7")
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if len(rec.NetworkConfigs) != 0 {
		t.Fatalf("record still carries %d NICs, want the stale NIC truncated", len(rec.NetworkConfigs))
	}
}

func TestConvergeOrphanedPause(t *testing.T) {
	var (
		mu      sync.Mutex
		resumes int
		state   = chStatePaused
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.resume", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		resumes++
		state = "Running"
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(chVMInfoResponse{State: state})
	})
	hc := newStubHTTPClient(t, mux)

	info, err := getVMInfo(t.Context(), hc)
	if err != nil {
		t.Fatalf("vm.info: %v", err)
	}
	fresh, err := convergeOrphanedPause(t.Context(), hc, "vm1", info)
	if err != nil {
		t.Fatalf("convergeOrphanedPause: %v", err)
	}
	if resumes != 1 {
		t.Fatalf("resumes = %d, want the orphaned pause resumed once", resumes)
	}

	if fresh.State != "Running" {
		t.Errorf("returned state = %q, want the refreshed Running", fresh.State)
	}
	if _, err = convergeOrphanedPause(t.Context(), hc, "vm1", fresh); err != nil {
		t.Fatalf("convergeOrphanedPause on a running VM: %v", err)
	}
	if resumes != 1 {
		t.Errorf("resumes = %d, want a running VM left untouched", resumes)
	}
}

type stubPlumbing struct {
	removed []int
}

func (p *stubPlumbing) Add(context.Context, string, *types.VMConfig, ...network.AddSpec) ([]*types.NetworkConfig, error) {
	return nil, nil
}

func (p *stubPlumbing) Remove(_ context.Context, _ string, indices ...int) error {
	p.removed = append(p.removed, indices...)
	return nil
}

func seedNetVM(t *testing.T, ch *CloudHypervisor, id string, nc *types.NetworkConfig) {
	t.Helper()
	ctx := t.Context()
	if err := ch.ReserveVM(ctx, id, &types.VMConfig{}, nil, ch.Conf.VMRunDir(id), ch.Conf.VMLogDir(id)); err != nil {
		t.Fatalf("seed reserve: %v", err)
	}
	if err := ch.UpdateRecord(ctx, id, func(r *hypervisor.VMRecord) error {
		r.State = types.VMStateCreated
		r.NetworkConfigs = []*types.NetworkConfig{nc}
		return nil
	}); err != nil {
		t.Fatalf("seed record: %v", err)
	}
}

func newTestCH(t *testing.T) *CloudHypervisor {
	t.Helper()
	conf := &config.Config{RootDir: t.TempDir(), RunDir: t.TempDir(), LogDir: t.TempDir()}
	cfg := NewConfig(conf)
	store, err := metajson.Open(metajson.Namespace{
		Name:     hypervisor.VMNamespaceName(typ),
		FilePath: NewConfig(conf).IndexFile(),
		LockPath: NewConfig(conf).IndexLock(),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{
			{Key: "vms", Table: hypervisor.TableRecords},
			{Key: "names", Table: hypervisor.TableNames},
			{Key: "tombstones", Table: tombstone.TableName, Optional: true},
		}},
	})
	if err != nil {
		t.Fatalf("meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	backend, err := hypervisor.NewBackend(typ, cfg, nil, store)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	return &CloudHypervisor{Backend: backend, conf: cfg}
}

func newStubHTTPClient(t *testing.T, mux *http.ServeMux) *http.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	return &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", addr)
		},
	}}
}

func newCHStubClient(t *testing.T, nets []chNet, stickyIDs ...string) (*http.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var removed []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.remove-device", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		removed = append(removed, req.ID)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		live := make([]chNet, 0, len(nets))
		tree := map[string]json.RawMessage{}
		for _, n := range nets {
			if slices.Contains(removed, n.ID) {
				if slices.Contains(stickyIDs, n.ID) {
					tree[n.ID] = json.RawMessage("{}")
				}
				continue
			}
			live = append(live, n)
			tree[n.ID] = json.RawMessage("{}")
		}
		_ = json.NewEncoder(w).Encode(chVMInfoResponse{Config: chVMInfoConfig{Nets: live}, DeviceTree: tree})
	})
	hc := newStubHTTPClient(t, mux)
	return hc, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(removed)
	}
}
