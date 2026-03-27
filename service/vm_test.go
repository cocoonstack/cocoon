package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/projecteru2/cocoon/types"
)

func TestCreateVM_Success(t *testing.T) {
	svc := newTestService(defaultHypervisor(), defaultImages(), defaultNetwork(), nil)

	vm, err := svc.CreateVM(context.Background(), validCreateParams())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vm == nil {
		t.Fatal("expected non-nil VM")
	}

	if vm.Config.Name != "test-vm" {
		t.Errorf("expected name test-vm, got %s", vm.Config.Name)
	}

	if vm.State != types.VMStateCreated {
		t.Errorf("expected state created, got %s", vm.State)
	}
}

func TestCreateVM_ImageNotFound(t *testing.T) {
	imgs := defaultImages()
	imgs.ConfigFunc = func(_ context.Context, _ []*types.VMConfig) ([][]*types.StorageConfig, []*types.BootConfig, error) {
		return nil, nil, fmt.Errorf("not found")
	}

	svc := newTestService(defaultHypervisor(), imgs, nil, nil)

	_, err := svc.CreateVM(context.Background(), validCreateParams())
	if err == nil {
		t.Fatal("expected error for missing image")
	}

	if !strings.Contains(err.Error(), "not resolved") {
		t.Errorf("expected 'not resolved' in error, got: %v", err)
	}
}

func TestCreateVM_NetworkFailure(t *testing.T) {
	net := defaultNetwork()
	net.ConfigFunc = func(_ context.Context, _ string, _ int, _ *types.VMConfig, _ ...*types.NetworkConfig) ([]*types.NetworkConfig, error) {
		return nil, fmt.Errorf("CNI plugin failed")
	}

	svc := newTestService(defaultHypervisor(), defaultImages(), net, nil)

	_, err := svc.CreateVM(context.Background(), validCreateParams())
	if err == nil {
		t.Fatal("expected error for network failure")
	}

	if !strings.Contains(err.Error(), "configure network") {
		t.Errorf("expected 'configure network' in error, got: %v", err)
	}
}

func TestCreateVM_HypervisorFailure_RollbacksNetwork(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.CreateFunc = func(_ context.Context, _ string, _ *types.VMConfig, _ []*types.StorageConfig, _ []*types.NetworkConfig, _ *types.BootConfig) (*types.VM, error) {
		return nil, fmt.Errorf("disk creation failed")
	}

	net := defaultNetwork()
	svc := newTestService(hyper, defaultImages(), net, nil)

	_, err := svc.CreateVM(context.Background(), validCreateParams())
	if err == nil {
		t.Fatal("expected error")
	}

	if len(net.DeleteCalls()) != 1 {
		t.Errorf("expected network rollback, got %d Delete calls", len(net.DeleteCalls()))
	}
}

func TestCreateVM_NoNICs(t *testing.T) {
	svc := newTestService(defaultHypervisor(), defaultImages(), nil, nil)

	p := validCreateParams()
	p.NICs = 0

	vm, err := svc.CreateVM(context.Background(), p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vm == nil {
		t.Fatal("expected non-nil VM")
	}
}

func TestCreateVM_ValidationFailure(t *testing.T) {
	svc := newTestService(defaultHypervisor(), defaultImages(), nil, nil)

	p := validCreateParams()
	p.CPU = 0 // invalid

	_, err := svc.CreateVM(context.Background(), p)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRunVM_Success(t *testing.T) {
	hyper := defaultHypervisor()
	svc := newTestService(hyper, defaultImages(), nil, nil)

	p := validCreateParams()
	p.NICs = 0

	vm, err := svc.RunVM(context.Background(), p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vm == nil {
		t.Fatal("expected non-nil VM")
	}

	if len(hyper.StartCalls()) != 1 {
		t.Errorf("expected Start called once, got %d", len(hyper.StartCalls()))
	}
}

func TestRunVM_StartFailure(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.StartFunc = func(_ context.Context, _ []string) ([]string, error) {
		return nil, fmt.Errorf("socket timeout")
	}

	svc := newTestService(hyper, defaultImages(), nil, nil)

	p := validCreateParams()
	p.NICs = 0

	_, err := svc.RunVM(context.Background(), p)
	if err == nil {
		t.Fatal("expected error on start failure")
	}

	if !strings.Contains(err.Error(), "start VM") {
		t.Errorf("expected 'start VM' in error, got: %v", err)
	}
}

func TestStartVM_Success(t *testing.T) {
	svc := newTestService(defaultHypervisor(), nil, nil, nil)

	started, err := svc.StartVM(context.Background(), []string{"vm-1", "vm-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(started) != 2 {
		t.Errorf("expected 2 started, got %d", len(started))
	}
}

func TestStartVM_WithNetworkRecovery(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.InspectFunc = func(_ context.Context, ref string) (*types.VM, error) {
		return &types.VM{
			ID:             ref,
			State:          types.VMStateCreated,
			Config:         types.VMConfig{Name: "test", CPU: 1, Memory: 512 << 20, Storage: 10 << 30},
			NetworkConfigs: []*types.NetworkConfig{{Tap: "tap0", Mac: "aa:bb:cc:dd:ee:ff"}},
		}, nil
	}

	net := defaultNetwork()
	net.VerifyFunc = func(_ context.Context, _ string) error {
		return fmt.Errorf("netns gone")
	}

	svc := newTestService(hyper, nil, net, nil)

	_, err := svc.StartVM(context.Background(), []string{"vm-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Network.Config should be called for recovery.
	if len(net.ConfigCalls()) != 1 {
		t.Errorf("expected 1 recovery Config call, got %d", len(net.ConfigCalls()))
	}
}

func TestStopVM_Success(t *testing.T) {
	svc := newTestService(defaultHypervisor(), nil, nil, nil)

	stopped, err := svc.StopVM(context.Background(), []string{"vm-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(stopped) != 1 {
		t.Errorf("expected 1 stopped, got %d", len(stopped))
	}
}

func TestListVM_Success(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.ListFunc = func(_ context.Context) ([]*types.VM, error) {
		return []*types.VM{
			{ID: "vm-2", CreatedAt: time.Now().Add(time.Second)},
			{ID: "vm-1", CreatedAt: time.Now()},
		}, nil
	}

	svc := newTestService(hyper, nil, nil, nil)

	vms, err := svc.ListVM(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(vms) != 2 {
		t.Errorf("expected 2 VMs, got %d", len(vms))
	}

	// Should be sorted by CreatedAt.
	if vms[0].ID != "vm-1" {
		t.Errorf("expected vm-1 first (older), got %s", vms[0].ID)
	}
}

func TestListVM_Empty(t *testing.T) {
	svc := newTestService(defaultHypervisor(), nil, nil, nil)

	vms, err := svc.ListVM(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(vms) != 0 {
		t.Errorf("expected 0 VMs, got %d", len(vms))
	}
}

func TestInspectVM_Success(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.InspectFunc = func(_ context.Context, ref string) (*types.VM, error) {
		return &types.VM{ID: ref, State: types.VMStateRunning}, nil
	}

	svc := newTestService(hyper, nil, nil, nil)

	vm, err := svc.InspectVM(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vm.ID != "vm-1" {
		t.Errorf("expected vm-1, got %s", vm.ID)
	}
}

func TestRemoveVM_Success(t *testing.T) {
	net := defaultNetwork()
	svc := newTestService(defaultHypervisor(), nil, net, nil)

	deleted, err := svc.RemoveVM(context.Background(), &VMRMParams{Refs: []string{"vm-1"}, Force: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted, got %d", len(deleted))
	}

	if len(net.DeleteCalls()) != 1 {
		t.Errorf("expected network cleanup, got %d Delete calls", len(net.DeleteCalls()))
	}
}

func TestRemoveVM_PartialFailure_StillCleansNetwork(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.DeleteFunc = func(_ context.Context, refs []string, _ bool) ([]string, error) {
		return refs[:1], fmt.Errorf("vm-2 not found")
	}

	net := defaultNetwork()
	svc := newTestService(hyper, nil, net, nil)

	deleted, err := svc.RemoveVM(context.Background(), &VMRMParams{Refs: []string{"vm-1", "vm-2"}})

	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted, got %d", len(deleted))
	}

	if len(net.DeleteCalls()) != 1 {
		t.Errorf("expected network cleanup for 1 VM, got %d calls", len(net.DeleteCalls()))
	}

	if err == nil {
		t.Error("expected error for partial failure")
	}
}

func TestCloneVM_StreamPath(t *testing.T) {
	snapCfg := &types.SnapshotConfig{
		ID:      "snap-1",
		Image:   "ubuntu:24.04",
		CPU:     2,
		Memory:  1 << 30,
		Storage: 10 << 30,
		NICs:    1,
	}

	snap := defaultSnapshot()
	snap.RestoreFunc = func(_ context.Context, _ string) (*types.SnapshotConfig, io.ReadCloser, error) {
		return snapCfg, io.NopCloser(strings.NewReader("")), nil
	}

	svc := newTestService(defaultHypervisor(), nil, defaultNetwork(), snap)

	vm, netConfigs, err := svc.CloneVM(context.Background(), &VMCloneParams{
		SnapshotRef: "snap-1",
		Name:        "clone-vm",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vm == nil {
		t.Fatal("expected non-nil VM")
	}

	if len(netConfigs) != 1 {
		t.Errorf("expected 1 network config, got %d", len(netConfigs))
	}
}

func TestCloneVM_NICsBelowMinimum(t *testing.T) {
	snapCfg := &types.SnapshotConfig{
		ID: "snap-1", Image: "ubuntu:24.04",
		CPU: 2, Memory: 1 << 30, Storage: 10 << 30, NICs: 2,
	}

	snap := defaultSnapshot()
	snap.RestoreFunc = func(_ context.Context, _ string) (*types.SnapshotConfig, io.ReadCloser, error) {
		return snapCfg, io.NopCloser(strings.NewReader("")), nil
	}

	svc := newTestService(defaultHypervisor(), nil, defaultNetwork(), snap)

	_, _, err := svc.CloneVM(context.Background(), &VMCloneParams{
		SnapshotRef: "snap-1",
		NICs:        1, // below snapshot's 2
	})
	if err == nil {
		t.Fatal("expected error for NICs below minimum")
	}

	if !strings.Contains(err.Error(), "below snapshot minimum") {
		t.Errorf("expected 'below snapshot minimum' in error, got: %v", err)
	}
}

func TestCloneVM_CPUBelowMinimum(t *testing.T) {
	snapCfg := &types.SnapshotConfig{
		ID: "snap-1", Image: "ubuntu:24.04",
		CPU: 4, Memory: 1 << 30, Storage: 10 << 30, NICs: 1,
	}

	snap := defaultSnapshot()
	snap.RestoreFunc = func(_ context.Context, _ string) (*types.SnapshotConfig, io.ReadCloser, error) {
		return snapCfg, io.NopCloser(strings.NewReader("")), nil
	}

	svc := newTestService(defaultHypervisor(), nil, defaultNetwork(), snap)

	_, _, err := svc.CloneVM(context.Background(), &VMCloneParams{
		SnapshotRef: "snap-1",
		CPU:         2, // below snapshot's 4
	})
	if err == nil {
		t.Fatal("expected error for CPU below minimum")
	}
}

func TestRestoreVM_Success(t *testing.T) {
	snapCfg := types.SnapshotConfig{
		ID: "snap-1", CPU: 2, Memory: 1 << 30, Storage: 10 << 30, NICs: 1,
	}

	hyper := defaultHypervisor()
	hyper.InspectFunc = func(_ context.Context, _ string) (*types.VM, error) {
		return &types.VM{
			ID:    "vm-1",
			State: types.VMStateRunning,
			Config: types.VMConfig{
				Name: "test", CPU: 2, Memory: 1 << 30, Storage: 10 << 30,
			},
			NetworkConfigs: []*types.NetworkConfig{{Tap: "tap0"}},
			SnapshotIDs:    map[string]struct{}{"snap-1": {}},
		}, nil
	}

	snap := defaultSnapshot()
	snap.InspectFunc = func(_ context.Context, _ string) (*types.Snapshot, error) {
		return &types.Snapshot{SnapshotConfig: snapCfg}, nil
	}
	snap.RestoreFunc = func(_ context.Context, _ string) (*types.SnapshotConfig, io.ReadCloser, error) {
		return &snapCfg, io.NopCloser(strings.NewReader("")), nil
	}

	svc := newTestService(hyper, nil, nil, snap)

	vm, err := svc.RestoreVM(context.Background(), &VMRestoreParams{
		VMRef:       "vm-1",
		SnapshotRef: "snap-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vm == nil {
		t.Fatal("expected non-nil VM")
	}
}

func TestRestoreVM_SnapshotNotOwned(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.InspectFunc = func(_ context.Context, _ string) (*types.VM, error) {
		return &types.VM{
			ID:             "vm-1",
			SnapshotIDs:    map[string]struct{}{"other-snap": {}},
			NetworkConfigs: []*types.NetworkConfig{{Tap: "tap0"}},
		}, nil
	}

	snap := defaultSnapshot()
	snap.InspectFunc = func(_ context.Context, _ string) (*types.Snapshot, error) {
		return &types.Snapshot{SnapshotConfig: types.SnapshotConfig{ID: "snap-1", NICs: 1}}, nil
	}

	svc := newTestService(hyper, nil, nil, snap)

	_, err := svc.RestoreVM(context.Background(), &VMRestoreParams{
		VMRef: "vm-1", SnapshotRef: "snap-1",
	})
	if err == nil {
		t.Fatal("expected error for snapshot not owned by VM")
	}

	if !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("expected 'does not belong' in error, got: %v", err)
	}
}

func TestRestoreVM_NICCountMismatch(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.InspectFunc = func(_ context.Context, _ string) (*types.VM, error) {
		return &types.VM{
			ID:             "vm-1",
			SnapshotIDs:    map[string]struct{}{"snap-1": {}},
			NetworkConfigs: []*types.NetworkConfig{{Tap: "tap0"}, {Tap: "tap1"}}, // 2 NICs
		}, nil
	}

	snap := defaultSnapshot()
	snap.InspectFunc = func(_ context.Context, _ string) (*types.Snapshot, error) {
		return &types.Snapshot{SnapshotConfig: types.SnapshotConfig{ID: "snap-1", NICs: 1}}, nil
	}

	svc := newTestService(hyper, nil, nil, snap)

	_, err := svc.RestoreVM(context.Background(), &VMRestoreParams{
		VMRef: "vm-1", SnapshotRef: "snap-1",
	})
	if err == nil {
		t.Fatal("expected error for NIC mismatch")
	}

	if !strings.Contains(err.Error(), "NIC count mismatch") {
		t.Errorf("expected 'NIC count mismatch' in error, got: %v", err)
	}
}

func TestRestoreVM_ResourceBelowMinimum(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.InspectFunc = func(_ context.Context, _ string) (*types.VM, error) {
		return &types.VM{
			ID: "vm-1",
			Config: types.VMConfig{
				Name: "test", CPU: 1, Memory: 512 << 20, Storage: 10 << 30,
			},
			SnapshotIDs:    map[string]struct{}{"snap-1": {}},
			NetworkConfigs: []*types.NetworkConfig{{Tap: "tap0"}},
		}, nil
	}

	snap := defaultSnapshot()
	snap.InspectFunc = func(_ context.Context, _ string) (*types.Snapshot, error) {
		return &types.Snapshot{SnapshotConfig: types.SnapshotConfig{
			ID: "snap-1", CPU: 4, Memory: 512 << 20, Storage: 10 << 30, NICs: 1,
		}}, nil
	}

	svc := newTestService(hyper, nil, nil, snap)

	_, err := svc.RestoreVM(context.Background(), &VMRestoreParams{
		VMRef: "vm-1", SnapshotRef: "snap-1",
	})
	if err == nil {
		t.Fatal("expected error for CPU below snapshot minimum")
	}

	if !strings.Contains(err.Error(), "below snapshot minimum") {
		t.Errorf("expected 'below snapshot minimum', got: %v", err)
	}
}

func TestDebugVM_Success(t *testing.T) {
	svc := newTestService(defaultHypervisor(), defaultImages(), nil, nil)

	info, err := svc.DebugVM(context.Background(), &DebugParams{
		VMCreateParams: *validCreateParams(),
		MaxCPU:         8,
		CHBin:          "cloud-hypervisor",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.BootConfig == nil {
		t.Error("expected non-nil BootConfig")
	}

	if info.VMConfig == nil {
		t.Error("expected non-nil VMConfig")
	}
}

// --- Clone config merge tests ---

func TestMergeCloneConfig_InheritDefaults(t *testing.T) {
	snapCfg := &types.SnapshotConfig{
		Image: "ubuntu:24.04", CPU: 2, Memory: 1 << 30, Storage: 10 << 30,
	}

	cfg, err := mergeCloneConfig(snapCfg, &VMCloneParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.CPU != 2 {
		t.Errorf("expected CPU=2, got %d", cfg.CPU)
	}

	if cfg.Memory != 1<<30 {
		t.Errorf("expected Memory=1G, got %d", cfg.Memory)
	}
}

func TestMergeCloneConfig_Override(t *testing.T) {
	snapCfg := &types.SnapshotConfig{
		Image: "ubuntu:24.04", CPU: 2, Memory: 1 << 30, Storage: 10 << 30,
	}

	cfg, err := mergeCloneConfig(snapCfg, &VMCloneParams{CPU: 4, Memory: 2 << 30})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.CPU != 4 {
		t.Errorf("expected CPU=4, got %d", cfg.CPU)
	}

	if cfg.Memory != 2<<30 {
		t.Errorf("expected Memory=2G, got %d", cfg.Memory)
	}
}

func TestMergeCloneConfig_BelowMinimum(t *testing.T) {
	snapCfg := &types.SnapshotConfig{CPU: 4, Memory: 2 << 30, Storage: 10 << 30}

	_, err := mergeCloneConfig(snapCfg, &VMCloneParams{CPU: 2})
	if err == nil {
		t.Fatal("expected error for CPU below minimum")
	}
}

// --- Restore config merge tests ---

func TestMergeRestoreConfig_KeepsCurrentValues(t *testing.T) {
	vm := &types.VM{
		Config: types.VMConfig{Name: "test", CPU: 4, Memory: 2 << 30, Storage: 20 << 30},
	}
	snapCfg := &types.SnapshotConfig{CPU: 2, Memory: 1 << 30, Storage: 10 << 30}

	cfg, err := mergeRestoreConfig(vm, snapCfg, &VMRestoreParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.CPU != 4 {
		t.Errorf("expected CPU=4, got %d", cfg.CPU)
	}
}

func TestMergeRestoreConfig_OverrideWithFlags(t *testing.T) {
	vm := &types.VM{
		Config: types.VMConfig{Name: "test", CPU: 2, Memory: 1 << 30, Storage: 10 << 30},
	}
	snapCfg := &types.SnapshotConfig{CPU: 2, Memory: 1 << 30, Storage: 10 << 30}

	cfg, err := mergeRestoreConfig(vm, snapCfg, &VMRestoreParams{CPU: 8})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.CPU != 8 {
		t.Errorf("expected CPU=8, got %d", cfg.CPU)
	}
}
