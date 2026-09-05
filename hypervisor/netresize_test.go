package hypervisor

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

func TestReconcileOrphanNICs(t *testing.T) {
	dev := &fakeNICDevices{live: []LiveNIC{
		{ID: "nic-01", MAC: "aa:bb:cc:dd:ee:01", TAP: "tapvm1beef-0"},
		{ID: "nic-02", MAC: "aa:bb:cc:dd:ee:02", TAP: "tapvm1beef-1"},
	}}
	plumbing := &stubPlumbing{}
	recorded := []*types.NetworkConfig{{MAC: "AA:BB:CC:DD:EE:01"}}
	if err := reconcileOrphanNICs(t.Context(), dev, "vm1", recorded, plumbing); err != nil {
		t.Fatalf("reconcileOrphanNICs: %v", err)
	}
	if !slices.Equal(dev.removed, []string{"nic-02"}) {
		t.Fatalf("removed = %v, want only the unrecorded NIC", dev.removed)
	}
	if !slices.Equal(plumbing.removed, []int{1}) {
		t.Fatalf("plumbing.removed = %v, want the orphan's host slot 1 reclaimed", plumbing.removed)
	}
}

func TestReconcileOrphanNICsReclaimsSlotWhenRemoveFails(t *testing.T) {
	dev := &fakeNICDevices{
		live:      []LiveNIC{{ID: "nic-02", MAC: "aa:bb:cc:dd:ee:02", TAP: "tapvm1beef-1"}},
		removeErr: errors.New("eject timeout"),
	}
	plumbing := &stubPlumbing{}
	if err := reconcileOrphanNICs(t.Context(), dev, "vm1", nil, plumbing); err == nil {
		t.Fatal("a failed device removal must surface an error")
	}
	if !slices.Equal(plumbing.removed, []int{1}) {
		t.Fatalf("plumbing.removed = %v, want host slot 1 reclaimed despite the failure", plumbing.removed)
	}
}

func TestResolveFailedPersist(t *testing.T) {
	b := newNetTestBackend(t)
	ctx := t.Context()
	nc := &types.NetworkConfig{MAC: "aa:bb:cc:dd:ee:01", TAP: "tap-vm1-0"}
	dev := &fakeNICDevices{}
	plumbing := &stubPlumbing{}
	seedNetVM(t, b, "vm1", nc)

	committed, err := b.resolveFailedPersist(ctx, dev, plumbing, "vm1", nc, "nic-01", 0)
	if err != nil || !committed {
		t.Fatalf("committed write must keep the device: committed=%v err=%v", committed, err)
	}
	if len(dev.removed) != 0 || len(plumbing.removed) != 0 {
		t.Fatal("no teardown may run when the record carries the NIC")
	}

	if err := b.UpdateRecord(ctx, "vm1", func(r *VMRecord) error {
		r.NetworkConfigs = nil
		return nil
	}); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	committed, err = b.resolveFailedPersist(ctx, dev, plumbing, "vm1", nc, "nic-01", 0)
	if err != nil || committed {
		t.Fatalf("conclusive miss must report uncommitted: committed=%v err=%v", committed, err)
	}
	if !slices.Equal(dev.removed, []string{"nic-01"}) || !slices.Equal(plumbing.removed, []int{0}) {
		t.Fatalf("removed=%v plumbing=%v, want the half-added device and nic 0 torn down", dev.removed, plumbing.removed)
	}
}

func TestNetResizeRemoveResumesWithoutLiveDevice(t *testing.T) {
	b := newNetTestBackend(t)
	ctx := t.Context()
	nc := &types.NetworkConfig{MAC: "aa:bb:cc:dd:ee:07", TAP: "tap-vm7-0"}
	seedNetVM(t, b, "vm7", nc)

	res, err := b.netResizeRemove(ctx, "vm7", []*types.NetworkConfig{nc}, &fakeNICDevices{}, &stubPlumbing{}, 0, netresize.Result{Before: 1, After: 1})
	if err != nil {
		t.Fatalf("resume remove must not error on a missing live device: %v", err)
	}
	if res.After != 0 || len(res.Removed) != 1 {
		t.Fatalf("res = %+v, want the NIC removed and After=0", res)
	}
	rec, err := b.PeekRecord(ctx, "vm7")
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if len(rec.NetworkConfigs) != 0 {
		t.Fatalf("record still carries %d NICs, want the stale NIC truncated", len(rec.NetworkConfigs))
	}
}

func TestNetResizeWithAddsAndPersists(t *testing.T) {
	b := newNetTestBackend(t)
	ctx := t.Context()
	seedNetVM(t, b, "vm2", &types.NetworkConfig{MAC: "aa:bb:cc:dd:ee:01", TAP: "tap-vm2-0"})
	rec, err := b.LoadRecord(ctx, "vm2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dev := &fakeNICDevices{live: []LiveNIC{{ID: "nic-01", MAC: "aa:bb:cc:dd:ee:01", TAP: "tap-vm2-0"}}}
	plumbing := &stubPlumbing{macs: []string{"aa:bb:cc:dd:ee:02", "aa:bb:cc:dd:ee:03"}}

	res, err := b.NetResizeWith(ctx, "vm2", &rec, dev, plumbing, 3)
	if err != nil {
		t.Fatalf("NetResizeWith: %v", err)
	}
	if res.Before != 1 || res.After != 3 || len(res.Added) != 2 || res.Added[1].Index != 2 {
		t.Fatalf("res = %+v, want two NICs added at indices 1 and 2", res)
	}
	if !slices.Equal(dev.added, []int{1, 2}) {
		t.Fatalf("device adds = %v, want indices 1 and 2", dev.added)
	}
	fresh, err := b.PeekRecord(ctx, "vm2")
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if len(fresh.NetworkConfigs) != 3 || fresh.NetworkConfigs[2].MAC != "aa:bb:cc:dd:ee:03" {
		t.Fatalf("record NICs = %+v, want three with the new MACs persisted", fresh.NetworkConfigs)
	}
}

func TestNetResizeAddRollsBackHostSlotOnDeviceFailure(t *testing.T) {
	b := newNetTestBackend(t)
	ctx := t.Context()
	seedNetVM(t, b, "vm3", &types.NetworkConfig{MAC: "aa:bb:cc:dd:ee:01", TAP: "tap-vm3-0"})
	rec, err := b.LoadRecord(ctx, "vm3")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dev := &fakeNICDevices{addErr: errors.New("vmm rejected")}
	plumbing := &stubPlumbing{macs: []string{"aa:bb:cc:dd:ee:02"}}
	if _, err := b.NetResizeWith(ctx, "vm3", &rec, dev, plumbing, 2); err == nil {
		t.Fatal("a device add failure must surface")
	}
	if !slices.Equal(plumbing.removed, []int{1}) {
		t.Fatalf("plumbing.removed = %v, want the fresh host slot 1 rolled back", plumbing.removed)
	}
	fresh, err := b.PeekRecord(ctx, "vm3")
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if len(fresh.NetworkConfigs) != 1 {
		t.Fatalf("record NICs = %d, want the failed NIC never persisted", len(fresh.NetworkConfigs))
	}
}

type fakeNICDevices struct {
	live      []LiveNIC
	added     []int
	removed   []string
	addErr    error
	removeErr error
}

func (f *fakeNICDevices) LiveNICs(context.Context) ([]LiveNIC, error) { return f.live, nil }

func (f *fakeNICDevices) AddNIC(_ context.Context, index int, nc *types.NetworkConfig) (string, error) {
	if f.addErr != nil {
		return "", f.addErr
	}
	f.added = append(f.added, index)
	id := "nic-" + nc.MAC
	f.live = append(f.live, LiveNIC{ID: id, MAC: nc.MAC, TAP: nc.TAP})
	return id, nil
}

func (f *fakeNICDevices) RemoveNIC(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return f.removeErr
}

type stubPlumbing struct {
	macs    []string
	removed []int
}

func (p *stubPlumbing) Add(_ context.Context, vmID string, _ *types.VMConfig, specs ...network.AddSpec) ([]*types.NetworkConfig, error) {
	out := make([]*types.NetworkConfig, 0, len(specs))
	for _, spec := range specs {
		mac := "de:ad:be:ef:00:00"
		if len(p.macs) > 0 {
			mac, p.macs = p.macs[0], p.macs[1:]
		}
		out = append(out, &types.NetworkConfig{TAP: network.TAPName("tap", vmID, spec.Index), MAC: mac})
	}
	return out, nil
}

func (p *stubPlumbing) Remove(_ context.Context, _ string, indices ...int) error {
	p.removed = append(p.removed, indices...)
	return nil
}

func newNetTestBackend(t *testing.T) *Backend {
	t.Helper()
	cfg := newDiskStubConfig(t)
	b, err := NewBackend("test-hv", cfg, nil, newTestMetaStore(t, "test-hv", cfg.indexFile, cfg.indexLock))
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	return b
}

func seedNetVM(t *testing.T, b *Backend, id string, nc *types.NetworkConfig) {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	if err := b.ReserveVM(ctx, id, &types.VMConfig{}, nil, filepath.Join(dir, "run"), filepath.Join(dir, "log")); err != nil {
		t.Fatalf("seed reserve: %v", err)
	}
	if err := b.UpdateRecord(ctx, id, func(r *VMRecord) error {
		r.State = types.VMStateCreated
		r.NetworkConfigs = []*types.NetworkConfig{nc}
		return nil
	}); err != nil {
		t.Fatalf("seed record: %v", err)
	}
}
