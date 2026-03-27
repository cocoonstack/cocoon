package service

import (
	"context"
	"io"
	"time"

	"github.com/projecteru2/cocoon/config"
	"github.com/projecteru2/cocoon/gc"
	"github.com/projecteru2/cocoon/hypervisor"
	imagebackend "github.com/projecteru2/cocoon/images"
	"github.com/projecteru2/cocoon/network"
	"github.com/projecteru2/cocoon/progress"
	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/types"
)

func testConfig() *config.Config {
	return &config.Config{
		RootDir: "/tmp/cocoon-test",
		RunDir:  "/tmp/cocoon-test/run",
		LogDir:  "/tmp/cocoon-test/log",
	}
}

// newTestService builds a Service with mock backends.
// Pass nil for any backend you don't need in a test.
func newTestService(hyper hypervisor.Hypervisor, imgs imagebackend.Images, net network.Network, snap snapshot.Snapshot) *Service {
	var images []imagebackend.Images
	if imgs != nil {
		images = append(images, imgs)
	}

	return NewWithBackends(testConfig(), hyper, images, net, snap)
}

// defaultHypervisor returns a HypervisorMock with sensible defaults.
// Override individual XxxFunc fields to customize behavior per test.
func defaultHypervisor() *HypervisorMock {
	return &HypervisorMock{
		TypeFunc: func() string { return "mock" },
		CreateFunc: func(_ context.Context, vmID string, vmCfg *types.VMConfig, _ []*types.StorageConfig, _ []*types.NetworkConfig, _ *types.BootConfig) (*types.VM, error) {
			return &types.VM{ID: vmID, State: types.VMStateCreated, Config: *vmCfg, CreatedAt: time.Now()}, nil
		},
		StartFunc: func(_ context.Context, refs []string) ([]string, error) {
			return refs, nil
		},
		StopFunc: func(_ context.Context, refs []string) ([]string, error) {
			return refs, nil
		},
		InspectFunc: func(_ context.Context, ref string) (*types.VM, error) {
			return &types.VM{ID: ref, State: types.VMStateRunning}, nil
		},
		ListFunc: func(_ context.Context) ([]*types.VM, error) {
			return nil, nil
		},
		DeleteFunc: func(_ context.Context, refs []string, _ bool) ([]string, error) {
			return refs, nil
		},
		ConsoleFunc: func(_ context.Context, _ string) (io.ReadWriteCloser, error) {
			return nil, nil
		},
		SnapshotFunc: func(_ context.Context, _ string) (*types.SnapshotConfig, io.ReadCloser, error) {
			return nil, nil, nil
		},
		CloneFunc: func(_ context.Context, vmID string, vmCfg *types.VMConfig, _ []*types.NetworkConfig, _ *types.SnapshotConfig, _ io.Reader) (*types.VM, error) {
			return &types.VM{ID: vmID, State: types.VMStateRunning, Config: *vmCfg}, nil
		},
		RestoreFunc: func(_ context.Context, vmRef string, vmCfg *types.VMConfig, _ io.Reader) (*types.VM, error) {
			return &types.VM{ID: vmRef, State: types.VMStateRunning, Config: *vmCfg}, nil
		},
		RegisterGCFunc: func(_ *gc.Orchestrator) {},
	}
}

// defaultImages returns an ImagesMock with sensible defaults.
func defaultImages() *ImagesMock {
	return &ImagesMock{
		TypeFunc: func() string { return "mock-images" },
		ConfigFunc: func(_ context.Context, vms []*types.VMConfig) ([][]*types.StorageConfig, []*types.BootConfig, error) {
			storages := make([][]*types.StorageConfig, len(vms))
			boots := make([]*types.BootConfig, len(vms))
			for i := range vms {
				storages[i] = []*types.StorageConfig{{Path: "/fake/disk.img", RO: true}}
				boots[i] = &types.BootConfig{KernelPath: "/fake/vmlinuz"}
			}
			return storages, boots, nil
		},
		PullFunc: func(_ context.Context, _ string, _ progress.Tracker) error {
			return nil
		},
		ImportFunc: func(_ context.Context, _ string, _ progress.Tracker, _ ...string) error {
			return nil
		},
		InspectFunc: func(_ context.Context, _ string) (*types.Image, error) {
			return nil, nil
		},
		ListFunc: func(_ context.Context) ([]*types.Image, error) {
			return nil, nil
		},
		DeleteFunc: func(_ context.Context, refs []string) ([]string, error) {
			return refs, nil
		},
		RegisterGCFunc: func(_ *gc.Orchestrator) {},
	}
}

// defaultNetwork returns a NetworkMock with sensible defaults.
func defaultNetwork() *NetworkMock {
	return &NetworkMock{
		TypeFunc: func() string { return "mock-network" },
		ConfigFunc: func(_ context.Context, _ string, numNICs int, _ *types.VMConfig, _ ...*types.NetworkConfig) ([]*types.NetworkConfig, error) {
			configs := make([]*types.NetworkConfig, numNICs)
			for i := range configs {
				configs[i] = &types.NetworkConfig{
					Tap: "tap0",
					Mac: "aa:bb:cc:dd:ee:ff",
					Network: &types.Network{
						IP:      "10.0.0.2",
						Gateway: "10.0.0.1",
						Prefix:  24,
					},
				}
			}
			return configs, nil
		},
		DeleteFunc: func(_ context.Context, vmIDs []string) ([]string, error) {
			return vmIDs, nil
		},
		VerifyFunc: func(_ context.Context, _ string) error {
			return nil
		},
		InspectFunc: func(_ context.Context, _ string) (*types.Network, error) {
			return nil, nil
		},
		ListFunc: func(_ context.Context) ([]*types.Network, error) {
			return nil, nil
		},
		RegisterGCFunc: func(_ *gc.Orchestrator) {},
	}
}

// defaultSnapshot returns a SnapshotMock with sensible defaults.
func defaultSnapshot() *SnapshotMock {
	return &SnapshotMock{
		TypeFunc: func() string { return "mock-snapshot" },
		CreateFunc: func(_ context.Context, _ *types.SnapshotConfig, _ io.Reader) (string, error) {
			return "snap-001", nil
		},
		ListFunc: func(_ context.Context) ([]*types.Snapshot, error) {
			return nil, nil
		},
		InspectFunc: func(_ context.Context, ref string) (*types.Snapshot, error) {
			return &types.Snapshot{SnapshotConfig: types.SnapshotConfig{ID: ref, Name: ref}}, nil
		},
		DeleteFunc: func(_ context.Context, refs []string) ([]string, error) {
			return refs, nil
		},
		RestoreFunc: func(_ context.Context, ref string) (*types.SnapshotConfig, io.ReadCloser, error) {
			return &types.SnapshotConfig{ID: ref}, io.NopCloser(nil), nil
		},
		RegisterGCFunc: func(_ *gc.Orchestrator) {},
	}
}

// validCreateParams returns a VMCreateParams that passes validation.
func validCreateParams() *VMCreateParams {
	return &VMCreateParams{
		Image:   "ubuntu:24.04",
		Name:    "test-vm",
		CPU:     2,
		Memory:  1 << 30,  // 1G
		Storage: 10 << 30, // 10G
		NICs:    1,
	}
}
