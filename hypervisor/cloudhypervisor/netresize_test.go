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

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// TestReconcileOrphanNICs covers the interrupted-resize fault window: a CH
// device whose MAC the VM record does not know must be ejected AND its host
// TAP slot reclaimed before a retry adds alongside it (a leftover bridge TAP
// wedges every retry); recorded and boot-time (_net*) devices must survive.
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
	recorded := []*types.NetworkConfig{{MAC: "AA:BB:CC:DD:EE:01"}} // case-insensitive match
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

// TestResolveFailedPersist covers the persist commit-ambiguity window: a
// committed write keeps the device; only a conclusive miss tears down.
func TestResolveFailedPersist(t *testing.T) {
	ch := newTestCH(t)
	ctx := t.Context()
	nc := &types.NetworkConfig{MAC: "aa:bb:cc:dd:ee:01", TAP: "tap-vm1-0"}
	hc, removed := newCHStubClient(t, []chNet{{ID: "cocoon-net-aabbccddee01", MAC: nc.MAC}})
	plumbing := &stubPlumbing{}

	if err := ch.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
		rec := &hypervisor.VMRecord{VM: types.VM{ID: "vm1", Hypervisor: ch.Typ}}
		rec.NetworkConfigs = []*types.NetworkConfig{nc}
		idx.VMs["vm1"] = rec
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	committed, err := ch.resolveFailedPersist(ctx, hc, plumbing, "vm1", nc, "cocoon-net-aabbccddee01", 0)
	if err != nil || !committed {
		t.Fatalf("committed write must keep the device: committed=%v err=%v", committed, err)
	}
	if len(removed()) != 0 || len(plumbing.removed) != 0 {
		t.Fatal("no teardown may run when the record carries the NIC")
	}

	if err := ch.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
		idx.VMs["vm1"].NetworkConfigs = nil
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

func TestNICPersisted(t *testing.T) {
	rec := &hypervisor.VMRecord{}
	rec.NetworkConfigs = []*types.NetworkConfig{{MAC: "AA:BB:CC:DD:EE:01"}}
	if !nicPersisted(rec, "aa:bb:cc:dd:ee:01") {
		t.Fatal("committed NIC must be detected case-insensitively (keep device, do not tear down)")
	}
	if nicPersisted(rec, "aa:bb:cc:dd:ee:02") {
		t.Fatal("an unpersisted MAC must roll back")
	}
	if nicPersisted(nil, "aa:bb:cc:dd:ee:01") {
		t.Fatal("a missing record is not a commit")
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

func newTestCH(t *testing.T) *CloudHypervisor {
	t.Helper()
	conf := &config.Config{RootDir: t.TempDir(), RunDir: t.TempDir(), LogDir: t.TempDir()}
	cfg := NewConfig(conf)
	backend, err := hypervisor.NewBackend(typ, cfg, nil)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	return &CloudHypervisor{Backend: backend, conf: cfg}
}

// newCHStubClient serves vm.info and vm.remove-device over an httptest server; removed() snapshots the eject calls.
func newCHStubClient(t *testing.T, nets []chNet) (*http.Client, func() []string) {
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
				continue
			}
			live = append(live, n)
			tree[n.ID] = json.RawMessage("{}")
		}
		_ = json.NewEncoder(w).Encode(chVMInfoResponse{Config: chVMInfoConfig{Nets: live}, DeviceTree: tree})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", addr)
		},
	}}
	return hc, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(removed)
	}
}
