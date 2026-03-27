package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	units "github.com/docker/go-units"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/projecteru2/cocoon/config"
	"github.com/projecteru2/cocoon/service"
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

// NewBaseHandler creates a BaseHandler that returns the given config pointer.
func NewBaseHandler(conf *config.Config) BaseHandler {
	return BaseHandler{ConfProvider: func() *config.Config { return conf }}
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
	backends, err := InitImageBackends(ctx, conf)
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
func InitImageBackends(ctx context.Context, conf *config.Config) ([]imagebackend.Images, error) {
	ociStore, err := oci.New(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("init oci backend: %w", err)
	}
	cloudimgStore, err := cloudimg.New(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("init cloudimg backend: %w", err)
	}
	return []imagebackend.Images{ociStore, cloudimgStore}, nil
}

// InitImageBackendsForPull returns concrete backend types needed by Pull.
func InitImageBackendsForPull(ctx context.Context, conf *config.Config) (*oci.OCI, *cloudimg.CloudImg, error) {
	ociStore, err := oci.New(ctx, conf)
	if err != nil {
		return nil, nil, fmt.Errorf("init oci backend: %w", err)
	}
	cloudimgStore, err := cloudimg.New(ctx, conf)
	if err != nil {
		return nil, nil, fmt.Errorf("init cloudimg backend: %w", err)
	}
	return ociStore, cloudimgStore, nil
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
	network, _ := cmd.Flags().GetString("network")

	if vmName == "" {
		vmName = sanitizeVMName(image)
	}

	memBytes, err := units.RAMInBytes(memStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --memory %q: %w", memStr, err)
	}
	storBytes, err := units.RAMInBytes(storStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --storage %q: %w", storStr, err)
	}

	cfg := &types.VMConfig{
		Name:    vmName,
		CPU:     cpu,
		Memory:  memBytes,
		Storage: storBytes,
		Image:   image,
		Network: network,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// CloneVMConfigFromFlags builds VMConfig for clone commands.
// Zero-value flags inherit from the snapshot config; explicit values are validated
// against the snapshot minimums (clone resources must be >= snapshot's).
func CloneVMConfigFromFlags(cmd *cobra.Command, snapCfg *types.SnapshotConfig) (*types.VMConfig, error) {
	vmName, _ := cmd.Flags().GetString("name")
	cpu, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")
	network, _ := cmd.Flags().GetString("network")

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

	cfg := &types.VMConfig{
		Name:    vmName,
		CPU:     cpu,
		Memory:  memBytes,
		Storage: storBytes,
		Image:   snapCfg.Image,
		Network: network,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// RestoreVMConfigFromFlags builds VMConfig for restore commands.
// Keeps VM's current values by default; CLI flags override.
// Validates that final values are >= snapshot minimums.
func RestoreVMConfigFromFlags(cmd *cobra.Command, vm *types.VM, snapCfg *types.SnapshotConfig) (*types.VMConfig, error) {
	cpu, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")

	result := vm.Config // value copy — keep current VM values

	if cpu > 0 {
		result.CPU = cpu
	}
	if memStr != "" {
		memBytes, err := units.RAMInBytes(memStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --memory %q: %w", memStr, err)
		}
		result.Memory = memBytes
	}
	if storStr != "" {
		storBytes, err := units.RAMInBytes(storStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --storage %q: %w", storStr, err)
		}
		result.Storage = storBytes
	}

	if result.CPU < snapCfg.CPU {
		return nil, fmt.Errorf("--cpu %d below snapshot minimum %d", result.CPU, snapCfg.CPU)
	}
	if result.Memory < snapCfg.Memory {
		return nil, fmt.Errorf("--memory %s below snapshot minimum %s", FormatSize(result.Memory), FormatSize(snapCfg.Memory))
	}
	if result.Storage < snapCfg.Storage {
		return nil, fmt.Errorf("--storage %s below snapshot minimum %s", FormatSize(result.Storage), FormatSize(snapCfg.Storage))
	}

	return &result, nil
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

// OutputJSON encodes v as indented JSON to stdout.
func OutputJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// AddFormatFlag registers the --format / -o flag on a command.
func AddFormatFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("format", "o", "table", `output format: "table" or "json"`)
}

// OutputFormatted checks --format flag: "json" → JSON, otherwise calls tableFn.
func OutputFormatted(cmd *cobra.Command, data any, tableFn func(w *tabwriter.Writer)) error {
	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		return OutputJSON(data)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	tableFn(w)
	return w.Flush()
}

func FormatSize(bytes int64) string {
	return units.HumanSize(float64(bytes))
}

func IsURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// --- Service integration ---

// InitService creates a Service from the command context and config.
func InitService(cmd *cobra.Command, conf *config.Config) (*service.Service, error) {
	return service.New(CommandContext(cmd), conf)
}

// --- Params-from-flags functions (CLI → service params) ---

// VMCreateParamsFromFlags builds VMCreateParams from cobra flags.
func VMCreateParamsFromFlags(cmd *cobra.Command, image string) (*service.VMCreateParams, error) {
	vmName, _ := cmd.Flags().GetString("name")
	cpu, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")
	nics, _ := cmd.Flags().GetInt("nics")
	network, _ := cmd.Flags().GetString("network")

	if vmName == "" {
		vmName = sanitizeVMName(image)
	}

	memBytes, err := units.RAMInBytes(memStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --memory %q: %w", memStr, err)
	}

	storBytes, err := units.RAMInBytes(storStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --storage %q: %w", storStr, err)
	}

	return &service.VMCreateParams{
		Image:   image,
		Name:    vmName,
		CPU:     cpu,
		Memory:  memBytes,
		Storage: storBytes,
		NICs:    nics,
		Network: network,
	}, nil
}

// VMCloneParamsFromFlags builds VMCloneParams from cobra flags.
func VMCloneParamsFromFlags(cmd *cobra.Command, snapshotRef string) (*service.VMCloneParams, error) {
	vmName, _ := cmd.Flags().GetString("name")
	cpu, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")
	nics, _ := cmd.Flags().GetInt("nics")
	network, _ := cmd.Flags().GetString("network")

	var memBytes int64
	if memStr != "" {
		var err error
		memBytes, err = units.RAMInBytes(memStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --memory %q: %w", memStr, err)
		}
	}

	var storBytes int64
	if storStr != "" {
		var err error
		storBytes, err = units.RAMInBytes(storStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --storage %q: %w", storStr, err)
		}
	}

	return &service.VMCloneParams{
		SnapshotRef: snapshotRef,
		Name:        vmName,
		CPU:         cpu,
		Memory:      memBytes,
		Storage:     storBytes,
		NICs:        nics,
		Network:     network,
	}, nil
}

// VMRestoreParamsFromFlags builds VMRestoreParams from cobra flags.
func VMRestoreParamsFromFlags(cmd *cobra.Command, vmRef, snapRef string) (*service.VMRestoreParams, error) {
	cpu, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")

	var memBytes int64
	if memStr != "" {
		var err error
		memBytes, err = units.RAMInBytes(memStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --memory %q: %w", memStr, err)
		}
	}

	var storBytes int64
	if storStr != "" {
		var err error
		storBytes, err = units.RAMInBytes(storStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --storage %q: %w", storStr, err)
		}
	}

	return &service.VMRestoreParams{
		VMRef:       vmRef,
		SnapshotRef: snapRef,
		CPU:         cpu,
		Memory:      memBytes,
		Storage:     storBytes,
	}, nil
}

// DebugParamsFromFlags builds DebugParams from cobra flags.
func DebugParamsFromFlags(cmd *cobra.Command, image string) (*service.DebugParams, error) {
	createParams, err := VMCreateParamsFromFlags(cmd, image)
	if err != nil {
		return nil, err
	}

	maxCPU, _ := cmd.Flags().GetInt("max-cpu")
	balloon, _ := cmd.Flags().GetInt("balloon")
	cowPath, _ := cmd.Flags().GetString("cow")
	chBin, _ := cmd.Flags().GetString("ch")

	return &service.DebugParams{
		VMCreateParams: *createParams,
		MaxCPU:         maxCPU,
		Balloon:        balloon,
		COWPath:        cowPath,
		CHBin:          chBin,
	}, nil
}

// sanitizeVMName derives a safe VM name from an image reference using
// go-containerregistry/pkg/name to properly parse registry, repository, tag,
// and digest components.
//
//	"ghcr.io/foo/ubuntu:24.04"        → "cocoon-foo-ubuntu-24.04"
//	"ubuntu:24.04"                    → "cocoon-ubuntu-24.04"
//	"ghcr.io/ns/img@sha256:abc..."    → "cocoon-ns-img"
func sanitizeVMName(image string) string {
	ref, err := name.ParseReference(image)
	if err != nil {
		// Unparseable — fall back to simple replace.
		n := strings.ReplaceAll(image, "/", "-")
		n = strings.ReplaceAll(n, ":", "-")
		n = "cocoon-" + n
		if len(n) > 63 {
			n = n[:63]
		}
		return n
	}

	// RepositoryStr() strips the registry hostname.
	// Docker Hub official images get "library/" prepended — strip it.
	repo := ref.Context().RepositoryStr()
	repo = strings.TrimPrefix(repo, "library/")

	n := "cocoon-" + strings.ReplaceAll(repo, "/", "-")

	// Append tag (but not digest — it's too long and not human-readable).
	if tag, ok := ref.(name.Tag); ok && tag.TagStr() != "latest" {
		n += "-" + tag.TagStr()
	}

	if len(n) > 63 {
		n = n[:63]
	}
	return n
}
