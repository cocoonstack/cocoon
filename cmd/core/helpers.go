package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/docker/go-units"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
	imagebackend "github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	"github.com/cocoonstack/cocoon/network"
	bridgenet "github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/network/cni"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/snapshot/localfile"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
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
	hyper, err := InitHypervisor(conf)
	if err != nil {
		return nil, nil, err
	}
	return backends, hyper, nil
}

// InitImageBackends initializes only image backends (no hypervisor needed).
func InitImageBackends(ctx context.Context, conf *config.Config) ([]imagebackend.Images, error) {
	ociStore, cloudimgStore, err := InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return nil, err
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

// InitHypervisor initializes the selected hypervisor backend.
func InitHypervisor(conf *config.Config) (hypervisor.Hypervisor, error) {
	ctor := findHypervisorFactory(conf.Hypervisor())
	if ctor == nil {
		return nil, fmt.Errorf("unknown hypervisor type: %s", conf.Hypervisor())
	}
	h, err := ctor(conf)
	if err != nil {
		return nil, fmt.Errorf("init hypervisor: %w", err)
	}
	return h, nil
}

// InitAllHypervisors initializes every backend in deterministic order.
func InitAllHypervisors(conf *config.Config) ([]hypervisor.Hypervisor, error) {
	result := make([]hypervisor.Hypervisor, 0, len(hypervisorFactories))
	for _, f := range hypervisorFactories {
		h, err := f.ctor(conf)
		if err != nil {
			return nil, fmt.Errorf("init %s for GC: %w", f.typ, err)
		}
		result = append(result, h)
	}
	return result, nil
}

// FindHypervisor returns the unique backend that owns ref.
func FindHypervisor(ctx context.Context, conf *config.Config, ref string) (hypervisor.Hypervisor, error) {
	hypers, err := InitAllHypervisors(conf)
	if err != nil {
		return nil, err
	}
	return resolveVMOwner(ctx, hypers, ref)
}

// ListAllVMs returns merged VM lists from all backends.
func ListAllVMs(ctx context.Context, hypers []hypervisor.Hypervisor) ([]*types.VM, error) {
	var all []*types.VM
	for _, h := range hypers {
		vms, listErr := h.List(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("list %s: %w", h.Type(), listErr)
		}
		all = append(all, vms...)
	}
	return all, nil
}

// RouteRefs groups VM refs by owning backend.
func RouteRefs(ctx context.Context, hypers []hypervisor.Hypervisor, refs []string) (map[hypervisor.Hypervisor][]string, error) {
	result := map[hypervisor.Hypervisor][]string{}
	for _, ref := range refs {
		owner, err := resolveVMOwner(ctx, hypers, ref)
		if err != nil {
			return nil, err
		}
		result[owner] = append(result[owner], ref)
	}
	return result, nil
}

// InitNetwork creates the CNI network provider.
func InitNetwork(conf *config.Config) (network.Network, error) {
	p, err := cni.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init network: %w", err)
	}
	return p, nil
}

// InitBridgeNetwork creates a TAP-on-bridge network provider.
func InitBridgeNetwork(conf *config.Config, bridgeDev string) (network.Network, error) {
	p, err := bridgenet.New(conf, bridgeDev)
	if err != nil {
		return nil, fmt.Errorf("init bridge network: %w", err)
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

// ResolveImage resolves vmCfg.Image across image backends.
func ResolveImage(ctx context.Context, backends []imagebackend.Images, vmCfg *types.VMConfig) ([]*types.StorageConfig, *types.BootConfig, error) {
	vms := []*types.VMConfig{vmCfg}
	var owner imagebackend.Images
	var storageConfigs []*types.StorageConfig
	var bootCfg *types.BootConfig
	var backendErrs []string
	for _, b := range backends {
		confs, boots, err := b.Config(ctx, vms)
		if err != nil {
			backendErrs = append(backendErrs, fmt.Sprintf("%s: %v", b.Type(), err))
			continue
		}
		if owner != nil {
			return nil, nil, fmt.Errorf("image %s: %w (matched both %s and %s)",
				vmCfg.Image, imagebackend.ErrAmbiguous, owner.Type(), b.Type())
		}
		owner = b
		storageConfigs = confs[0]
		bootCfg = boots[0]
	}
	if owner == nil {
		return nil, nil, fmt.Errorf("image %q not resolved: %s", vmCfg.Image, strings.Join(backendErrs, "; "))
	}
	return storageConfigs, bootCfg, nil
}

// ResolveImageOwner returns the single backend that owns ref.
func ResolveImageOwner(ctx context.Context, backends []imagebackend.Images, ref string) (imagebackend.Images, error) {
	var matches []imagebackend.Images
	for _, b := range backends {
		img, err := b.Inspect(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("inspect %s in %s: %w", ref, b.Type(), err)
		}
		if img != nil {
			matches = append(matches, b)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("image %q: not found in any backend", ref)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, b := range matches {
			names[i] = b.Type()
		}
		return nil, fmt.Errorf("image %s: %w (backends: %s)", ref, imagebackend.ErrAmbiguous, strings.Join(names, ", "))
	}
}

// VMConfigFromFlags builds VMConfig for create/run commands.
func VMConfigFromFlags(cmd *cobra.Command, image string) (*types.VMConfig, error) {
	vmName, _ := cmd.Flags().GetString("name")
	cpu, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")
	queueSize, _ := cmd.Flags().GetInt("queue-size")
	diskQueueSize, _ := cmd.Flags().GetInt("disk-queue-size")
	network, _ := cmd.Flags().GetString("network")
	user, _ := cmd.Flags().GetString("user")
	password, _ := cmd.Flags().GetString("password")
	noDirectIO, _ := cmd.Flags().GetBool("no-direct-io")
	windows, _ := cmd.Flags().GetBool("windows")

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
		Name:          vmName,
		CPU:           cpu,
		Memory:        memBytes,
		Storage:       storBytes,
		QueueSize:     queueSize,
		DiskQueueSize: diskQueueSize,
		Image:         image,
		Network:       network,
		User:          user,
		Password:      password,
		NoDirectIO:    noDirectIO,
		Windows:       windows,
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
	network, _ := cmd.Flags().GetString("network")
	if network == "" {
		network = snapCfg.Network
	}
	queueSize, _ := cmd.Flags().GetInt("queue-size")
	if queueSize == 0 {
		queueSize = snapCfg.QueueSize
	}
	diskQueueSize, _ := cmd.Flags().GetInt("disk-queue-size")
	if diskQueueSize == 0 {
		diskQueueSize = snapCfg.DiskQueueSize
	}
	noDirectIO := snapCfg.NoDirectIO
	if cmd.Flags().Changed("no-direct-io") {
		noDirectIO, _ = cmd.Flags().GetBool("no-direct-io")
	}

	cpu, memBytes, storBytes, err := mergeResourceFlags(cmd, snapCfg.CPU, snapCfg.Memory, snapCfg.Storage, snapCfg)
	if err != nil {
		return nil, err
	}

	onDemand, _ := cmd.Flags().GetBool("on-demand")

	cfg := &types.VMConfig{
		Name:          vmName,
		CPU:           cpu,
		Memory:        memBytes,
		Storage:       storBytes,
		QueueSize:     queueSize,
		DiskQueueSize: diskQueueSize,
		Image:         snapCfg.Image,
		ImageDigest:   snapCfg.ImageDigest,
		ImageType:     snapCfg.ImageType,
		Network:       network,
		NoDirectIO:    noDirectIO,
		Windows:       snapCfg.Windows,
		OnDemand:      onDemand,
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
	result := vm.Config // value copy — keep current VM values

	cpu, memBytes, storBytes, err := mergeResourceFlags(cmd, result.CPU, result.Memory, result.Storage, snapCfg)
	if err != nil {
		return nil, err
	}
	result.CPU = cpu
	result.Memory = memBytes
	result.Storage = storBytes

	onDemand, _ := cmd.Flags().GetBool("on-demand")
	result.OnDemand = onDemand

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

// FormatSize formats a byte count as a human-readable size string.
func FormatSize(bytes int64) string {
	return units.HumanSize(float64(bytes))
}

// IsURL reports whether ref starts with http:// or https://.
func IsURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// hypervisorFactory keeps backend lookup and iteration order together.
type hypervisorFactory struct {
	typ  config.HypervisorType
	ctor func(*config.Config) (hypervisor.Hypervisor, error)
}

var hypervisorFactories = []hypervisorFactory{
	{config.HypervisorCH, func(c *config.Config) (hypervisor.Hypervisor, error) { return cloudhypervisor.New(c) }},
	{config.HypervisorFirecracker, func(c *config.Config) (hypervisor.Hypervisor, error) { return firecracker.New(c) }},
}

// findHypervisorFactory returns the ctor for a given type, or nil.
func findHypervisorFactory(typ config.HypervisorType) func(*config.Config) (hypervisor.Hypervisor, error) {
	for _, f := range hypervisorFactories {
		if f.typ == typ {
			return f.ctor
		}
	}
	return nil
}

// resolveVMOwner returns the single backend that owns ref.
func resolveVMOwner(ctx context.Context, hypers []hypervisor.Hypervisor, ref string) (hypervisor.Hypervisor, error) {
	var matches []hypervisor.Hypervisor
	for _, h := range hypers {
		_, resolveErr := h.Inspect(ctx, ref)
		if resolveErr == nil {
			matches = append(matches, h)
			continue
		}
		if errors.Is(resolveErr, hypervisor.ErrNotFound) {
			continue
		}
		return nil, fmt.Errorf("inspect %s in %s: %w", ref, h.Type(), resolveErr)
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("VM %s: %w", ref, hypervisor.ErrNotFound)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, h := range matches {
			names[i] = h.Type()
		}
		return nil, fmt.Errorf("VM %s: %w (backends: %s)", ref, hypervisor.ErrAmbiguous, strings.Join(names, ", "))
	}
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

func mergeResourceFlags(cmd *cobra.Command, cpu int, memory, storage int64, snapCfg *types.SnapshotConfig) (int, int64, int64, error) {
	cpuFlag, _ := cmd.Flags().GetInt("cpu")
	memStr, _ := cmd.Flags().GetString("memory")
	storStr, _ := cmd.Flags().GetString("storage")

	if cpuFlag > 0 {
		cpu = cpuFlag
	}
	if memStr != "" {
		v, err := units.RAMInBytes(memStr)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("invalid --memory %q: %w", memStr, err)
		}
		memory = v
	}
	if storStr != "" {
		v, err := units.RAMInBytes(storStr)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("invalid --storage %q: %w", storStr, err)
		}
		storage = v
	}

	if cpu < snapCfg.CPU {
		return 0, 0, 0, fmt.Errorf("--cpu %d below snapshot minimum %d", cpu, snapCfg.CPU)
	}
	if memory < snapCfg.Memory {
		return 0, 0, 0, fmt.Errorf("--memory %s below snapshot minimum %s", FormatSize(memory), FormatSize(snapCfg.Memory))
	}
	if storage < snapCfg.Storage {
		return 0, 0, 0, fmt.Errorf("--storage %s below snapshot minimum %s", FormatSize(storage), FormatSize(snapCfg.Storage))
	}
	return cpu, memory, storage, nil
}
