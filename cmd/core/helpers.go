package core

import (
	"context"
	"fmt"
	"strings"

	units "github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/projecteru2/cocoon/config"
	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/hypervisor/cloudhypervisor"
	imagebackend "github.com/projecteru2/cocoon/images"
	"github.com/projecteru2/cocoon/images/cloudimg"
	"github.com/projecteru2/cocoon/images/oci"
	"github.com/projecteru2/cocoon/network"
	"github.com/projecteru2/cocoon/network/cni"
	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/snapshot/localfile"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

// BaseHandler provides shared config access for all command handlers.
type BaseHandler struct {
	ConfProvider func() *config.Config
}

// Init returns the command context and validated config in one call.
func (h BaseHandler) Init(cmd *cobra.Command) (context.Context, *config.Config, error) {
	conf, err := h.Conf()
	if err != nil {
		return nil, nil, err
	}
	return CommandContext(cmd), conf, nil
}

// Conf validates and returns the config. All handlers call this first.
func (h BaseHandler) Conf() (*config.Config, error) {
	if h.ConfProvider == nil {
		return nil, fmt.Errorf("config provider is nil")
	}
	conf := h.ConfProvider()
	if conf == nil {
		return nil, fmt.Errorf("config not initialized")
	}
	return conf, nil
}

// CommandContext returns command context, falling back to Background.
func CommandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

// InitBackends initializes all image backends and the hypervisor.
func InitBackends(ctx context.Context, conf *config.Config) ([]imagebackend.Images, hypervisor.Hypervisor, error) {
	backends, _, _, err := InitImageBackends(ctx, conf)
	if err != nil {
		return nil, nil, err
	}
	ch, err := cloudhypervisor.New(conf)
	if err != nil {
		return nil, nil, fmt.Errorf("init hypervisor: %w", err)
	}
	return backends, ch, nil
}

// InitImageBackends initializes only image backends (no hypervisor needed).
func InitImageBackends(ctx context.Context, conf *config.Config) ([]imagebackend.Images, *oci.OCI, *cloudimg.CloudImg, error) {
	ociStore, err := oci.New(ctx, conf)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("init oci backend: %w", err)
	}
	cloudimgStore, err := cloudimg.New(ctx, conf)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("init cloudimg backend: %w", err)
	}
	return []imagebackend.Images{ociStore, cloudimgStore}, ociStore, cloudimgStore, nil
}

// InitHypervisor initializes only the hypervisor.
func InitHypervisor(conf *config.Config) (hypervisor.Hypervisor, error) {
	ch, err := cloudhypervisor.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init hypervisor: %w", err)
	}
	return ch, nil
}

// InitNetwork creates the CNI network provider.
func InitNetwork(conf *config.Config) (network.Network, error) {
	p, err := cni.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init network: %w", err)
	}
	return p, nil
}

// InitSnapshot initializes the snapshot backend.
func InitSnapshot(conf *config.Config) (snapshot.Snapshot, error) {
	s, err := localfile.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init snapshot backend: %w", err)
	}
	return s, nil
}

// ResolveImage resolves an image reference to StorageConfigs + BootConfig.
func ResolveImage(ctx context.Context, backends []imagebackend.Images, vmCfg *types.VMConfig) ([]*types.StorageConfig, *types.BootConfig, error) {
	vms := []*types.VMConfig{vmCfg}
	var storageConfigs []*types.StorageConfig
	var bootCfg *types.BootConfig
	var backendErrs []string
	for _, b := range backends {
		confs, boots, err := b.Config(ctx, vms)
		if err != nil {
			backendErrs = append(backendErrs, fmt.Sprintf("%s: %v", b.Type(), err))
			continue
		}
		storageConfigs = confs[0]
		bootCfg = boots[0]
		break
	}
	if bootCfg == nil {
		return nil, nil, fmt.Errorf("image %q not resolved: %s", vmCfg.Image, strings.Join(backendErrs, "; "))
	}
	return storageConfigs, bootCfg, nil
}

// VMConfigFromFlags builds VMConfig for create/run commands.
func VMConfigFromFlags(cmd *cobra.Command, image string) (*types.VMConfig, error) {
	vmName, _ := cmd.Flags().GetString("name")
	cpu, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")

	if vmName == "" {
		vmName = sanitizeVMName(image)
	}

	if cpu <= 0 {
		return nil, fmt.Errorf("--cpu must be at least 1")
	}

	memBytes, err := units.RAMInBytes(memStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --memory %q: %w", memStr, err)
	}
	storBytes, err := units.RAMInBytes(storStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --storage %q: %w", storStr, err)
	}

	return &types.VMConfig{
		Name:    vmName,
		CPU:     cpu,
		Memory:  memBytes,
		Storage: storBytes,
		Image:   image,
	}, nil
}

// CloneVMConfigFromFlags builds VMConfig for clone commands.
// Zero-value flags inherit from the snapshot config; explicit values are validated
// against the snapshot minimums (clone resources must be >= snapshot's).
func CloneVMConfigFromFlags(cmd *cobra.Command, snapCfg *types.SnapshotConfig) (*types.VMConfig, error) {
	vmName, _ := cmd.Flags().GetString("name")
	cpu, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")

	if cpu == 0 {
		cpu = snapCfg.CPU
	}

	var memBytes int64
	if memStr == "" {
		memBytes = snapCfg.Memory
	} else {
		var err error
		memBytes, err = units.RAMInBytes(memStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --memory %q: %w", memStr, err)
		}
	}

	var storBytes int64
	if storStr == "" {
		storBytes = snapCfg.Storage
	} else {
		var err error
		storBytes, err = units.RAMInBytes(storStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --storage %q: %w", storStr, err)
		}
	}

	if cpu < snapCfg.CPU {
		return nil, fmt.Errorf("--cpu %d below snapshot minimum %d", cpu, snapCfg.CPU)
	}
	if memBytes < snapCfg.Memory {
		return nil, fmt.Errorf("--memory %s below snapshot minimum %s", FormatSize(memBytes), FormatSize(snapCfg.Memory))
	}
	if storBytes < snapCfg.Storage {
		return nil, fmt.Errorf("--storage %s below snapshot minimum %s", FormatSize(storBytes), FormatSize(snapCfg.Storage))
	}

	return &types.VMConfig{
		Name:    vmName,
		CPU:     cpu,
		Memory:  memBytes,
		Storage: storBytes,
		Image:   snapCfg.Image,
	}, nil
}

// CloneNICsFromFlags returns the NIC count for clone.
// 0 (default) inherits from the snapshot; an explicit different value is rejected
// because the NIC count must match the snapshot's device tree.
func CloneNICsFromFlags(cmd *cobra.Command, snapCfg *types.SnapshotConfig) (int, error) {
	nics, _ := cmd.Flags().GetInt("nics")
	if nics == 0 {
		return snapCfg.NICs, nil
	}
	if nics != snapCfg.NICs {
		return 0, fmt.Errorf("--nics %d differs from snapshot's %d (device tree must match)", nics, snapCfg.NICs)
	}
	return nics, nil
}

// EnsureFirmwarePath sets default firmware path for cloudimg boot.
func EnsureFirmwarePath(conf *config.Config, bootCfg *types.BootConfig) {
	if bootCfg != nil && bootCfg.KernelPath == "" && bootCfg.FirmwarePath == "" {
		bootCfg.FirmwarePath = cloudimg.NewConfig(conf).FirmwarePath()
	}
}

// ReconcileState checks actual process liveness to detect stale "running" records.
func ReconcileState(vm *types.VM) string {
	if vm.State == types.VMStateRunning && !utils.IsProcessAlive(vm.PID) {
		return "stopped (stale)"
	}
	return string(vm.State)
}

func FormatSize(bytes int64) string {
	return units.HumanSize(float64(bytes))
}

func IsURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// sanitizeVMName derives a safe VM name from an image reference.
// Strips registry prefix (everything before the last '/'), replaces ':' with '-',
// and prepends "cocoon-".
// e.g. "ghcr.io/foo/ubuntu:24.04" → "cocoon-ubuntu-24.04"
func sanitizeVMName(image string) string {
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ReplaceAll(name, ":", "-")
	return "cocoon-" + name
}
