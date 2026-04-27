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
	"github.com/projecteru2/core/log"
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
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/snapshot/localfile"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// hypervisorFactory keeps backend lookup and iteration order together.
type hypervisorFactory struct {
	typ  config.HypervisorType
	ctor func(*config.Config) (hypervisor.Hypervisor, error)
}

var hypervisorFactories = []hypervisorFactory{
	{config.HypervisorCH, func(c *config.Config) (hypervisor.Hypervisor, error) { return cloudhypervisor.New(c) }},
	{config.HypervisorFirecracker, func(c *config.Config) (hypervisor.Hypervisor, error) { return firecracker.New(c) }},
}

// BaseHandler provides shared config access for all command handlers.
type BaseHandler struct {
	ConfProvider func() *config.Config
}

func NewBaseHandler(conf *config.Config) BaseHandler {
	return BaseHandler{ConfProvider: func() *config.Config { return conf }}
}

// Init returns command context and validated config.
func (h BaseHandler) Init(cmd *cobra.Command) (context.Context, *config.Config, error) {
	conf, err := h.Conf()
	if err != nil {
		return nil, nil, err
	}
	return CommandContext(cmd), conf, nil
}

// Conf validates and returns the config.
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

// CommandContext returns command context, fallback to Background.
func CommandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

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

func InitImageBackends(ctx context.Context, conf *config.Config) ([]imagebackend.Images, error) {
	ociStore, cloudimgStore, err := InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return nil, err
	}
	return []imagebackend.Images{ociStore, cloudimgStore}, nil
}

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

func FindHypervisor(ctx context.Context, conf *config.Config, ref string) (hypervisor.Hypervisor, error) {
	hypers, err := InitAllHypervisors(conf)
	if err != nil {
		return nil, err
	}
	return resolveVMOwner(ctx, hypers, ref)
}

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

func InitNetwork(conf *config.Config) (network.Network, error) {
	p, err := cni.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init network: %w", err)
	}
	return p, nil
}

func InitBridgeNetwork(conf *config.Config, bridgeDev string) (network.Network, error) {
	p, err := bridgenet.New(conf, bridgeDev)
	if err != nil {
		return nil, fmt.Errorf("init bridge network: %w", err)
	}
	return p, nil
}

func InitSnapshot(conf *config.Config) (snapshot.Snapshot, error) {
	s, err := localfile.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init snapshot backend: %w", err)
	}
	return s, nil
}

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

// EnsureImage checks whether the exact base image (by digest) required by
// vmCfg exists locally and pulls it if missing. Inspect uses ImageDigest
// to match the exact version recorded at snapshot time; Pull uses Image
// (tag/URL) as the registry reference, then verifies the pulled digest.
//
// For imported images (not pullable from a registry), a warning is logged
// and the caller proceeds — VerifyBaseFiles will catch the actual error.
func EnsureImage(ctx context.Context, backends []imagebackend.Images, vmCfg *types.VMConfig) {
	if vmCfg.Image == "" || vmCfg.ImageType == "" {
		return
	}
	logger := log.WithFunc("core.EnsureImage")

	// Use digest for lookup when available; fall back to tag/URL.
	lookupRef := vmCfg.ImageDigest
	if lookupRef == "" {
		lookupRef = vmCfg.Image
	}

	for _, b := range backends {
		if b.Type() != vmCfg.ImageType {
			continue
		}
		img, inspectErr := b.Inspect(ctx, lookupRef)
		if inspectErr != nil {
			logger.Warnf(ctx, "inspect image %s: %v — will attempt pull", lookupRef, inspectErr)
		}
		if img != nil {
			return // exact image version exists locally
		}
		// Pull by digest reference when available — ensures we get the exact
		// version recorded at snapshot time, not whatever the tag points to now.
		pullRef := digestPullRef(vmCfg.Image, vmCfg.ImageDigest, vmCfg.ImageType)
		logger.Infof(ctx, "base image not found locally, pulling %s ...", pullRef)
		if pullErr := b.Pull(ctx, pullRef, false, progress.Nop); pullErr != nil {
			logger.Warnf(ctx, "auto-pull %s failed (imported image?): %v — clone may fail if base layers are missing", pullRef, pullErr)
			return
		}
		// For non-OCI images (e.g. cloudimg), digestPullRef cannot pin by
		// digest, so the pull may fetch newer content whose digest differs
		// from the one recorded at snapshot time. Verify post-pull.
		if vmCfg.ImageDigest != "" && pullRef == vmCfg.Image {
			img, err := b.Inspect(ctx, vmCfg.ImageDigest)
			if err != nil || img == nil {
				logger.Warnf(ctx, "pulled %s but expected digest %s not found locally — clone may fail in VerifyBaseFiles", pullRef, vmCfg.ImageDigest)
				return
			}
		}
		logger.Infof(ctx, "base image %s pulled successfully", pullRef)
		return
	}
}

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
	dataDiskRaw, _ := cmd.Flags().GetStringArray("data-disk")

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

	dataDisks, err := parseDataDiskFlags(dataDiskRaw)
	if err != nil {
		return nil, err
	}

	cfg := &types.VMConfig{
		Name: vmName,
		Config: types.Config{
			CPU:           cpu,
			Memory:        memBytes,
			Storage:       storBytes,
			QueueSize:     queueSize,
			DiskQueueSize: diskQueueSize,
			Image:         image,
			Network:       network,
			NoDirectIO:    noDirectIO,
			Windows:       windows,
		},
		User:      user,
		Password:  password,
		DataDisks: dataDisks,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// parseDataDiskFlags parses --data-disk values, normalizes defaults, and
// returns the spec list ready for hypervisor.PrepareDataDisks.
func parseDataDiskFlags(raw []string) ([]types.DataDiskSpec, error) {
	specs := make([]types.DataDiskSpec, 0, len(raw))
	for _, s := range raw {
		spec, err := parseDataDiskSpec(s)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	if err := normalizeDataDiskSpecs(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// parseDataDiskSpec parses one --data-disk argument: comma-separated key=value
// pairs. size is required and >= 16MiB; name/fstype/mount/directio are
// optional and defaulted by normalizeDataDiskSpecs.
func parseDataDiskSpec(s string) (types.DataDiskSpec, error) {
	var spec types.DataDiskSpec
	if s == "" {
		return spec, fmt.Errorf("--data-disk: empty spec")
	}
	for part := range strings.SplitSeq(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return spec, fmt.Errorf("--data-disk: %q is not key=value", part)
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		switch key {
		case "size":
			n, err := units.RAMInBytes(val)
			if err != nil {
				return spec, fmt.Errorf("--data-disk: invalid size %q: %w", val, err)
			}
			if n < hypervisor.MinDataDiskSize {
				return spec, fmt.Errorf("--data-disk: size %s below 16MiB minimum", val)
			}
			spec.Size = n
		case "name":
			if !types.ValidDataDiskName(val) {
				return spec, fmt.Errorf("--data-disk: invalid name %q (must match [a-z][a-z0-9_-]{0,19}, no cocoon- prefix)", val)
			}
			spec.Name = val
		case "fstype":
			if val != "ext4" && val != "none" {
				return spec, fmt.Errorf("--data-disk: unsupported fstype %q (only ext4, none in Phase 1)", val)
			}
			spec.FSType = val
		case "mount":
			spec.MountPoint = val
			spec.MountPointSet = true
		case "directio":
			switch val {
			case "on":
				t := true
				spec.DirectIO = &t
			case "off":
				f := false
				spec.DirectIO = &f
			case "auto":
				// keep nil to inherit VM-level NoDirectIO
			default:
				return spec, fmt.Errorf("--data-disk: directio must be on/off/auto, got %q", val)
			}
		default:
			return spec, fmt.Errorf("--data-disk: unknown key %q", key)
		}
	}
	if spec.Size == 0 {
		return spec, fmt.Errorf("--data-disk: size= required")
	}
	return spec, nil
}

// normalizeDataDiskSpecs fills defaults and enforces uniqueness:
//   - empty FSType => ext4 (then re-validates whitelist)
//   - empty Name => dataN, skipping any name already taken
//   - !MountPointSet && fstype != none => /mnt/<name>
//   - fstype == none requires MountPoint == ""
//   - Names must be unique across the batch
func normalizeDataDiskSpecs(specs []types.DataDiskSpec) error {
	used := make(map[string]bool)
	for _, s := range specs {
		if s.Name == "" {
			continue
		}
		if used[s.Name] {
			return fmt.Errorf("--data-disk: name %q duplicated", s.Name)
		}
		used[s.Name] = true
	}
	autoIdx := 1
	for i := range specs {
		if specs[i].FSType == "" {
			specs[i].FSType = "ext4"
		}
		if specs[i].FSType != "ext4" && specs[i].FSType != "none" {
			return fmt.Errorf("--data-disk: invalid fstype %q", specs[i].FSType)
		}
		if specs[i].Name == "" {
			for {
				candidate := fmt.Sprintf("data%d", autoIdx)
				autoIdx++
				if !used[candidate] {
					specs[i].Name = candidate
					used[candidate] = true
					break
				}
			}
		}
		if !specs[i].MountPointSet && specs[i].FSType != "none" {
			specs[i].MountPoint = "/mnt/" + specs[i].Name
		}
		if specs[i].FSType == "none" && specs[i].MountPoint != "" {
			return fmt.Errorf("--data-disk %s: fstype=none requires empty mount", specs[i].Name)
		}
	}
	return nil
}

// CloneVMConfigFromFlags builds VMConfig for clone (inherits from snapshot).
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
		Name: vmName,
		Config: types.Config{
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
		},
		OnDemand: onDemand,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// RestoreVMConfigFromFlags builds VMConfig for restore (allows overrides).
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

func EnsureFirmwarePath(conf *config.Config, bootCfg *types.BootConfig) {
	if bootCfg != nil && bootCfg.KernelPath == "" && bootCfg.FirmwarePath == "" {
		bootCfg.FirmwarePath = cloudimg.NewConfig(conf).FirmwarePath()
	}
}

// ReconcileState detects stale running records via process liveness.
func ReconcileState(vm *types.VM) string {
	if vm.State == types.VMStateRunning && !utils.IsProcessAlive(vm.PID) {
		return "stopped (stale)"
	}
	return string(vm.State)
}

func OutputJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func AddFormatFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("format", "o", "table", `output format: "table" or "json"`)
}

// AddOutputFlag adds --output/-o for lifecycle commands. Empty default keeps
// the human-readable log output; "json" emits a parseable result on stdout.
func AddOutputFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("output", "o", "", `emit "json" for machine-readable output`)
}

// WantJSON reports whether --output=json was requested.
func WantJSON(cmd *cobra.Command) bool {
	out, _ := cmd.Flags().GetString("output")
	return out == "json"
}

// MaybeOutputJSON emits v as JSON iff --output=json is set. Returns
// (true, encodeErr) when JSON was emitted — caller should return the error
// without falling through to human-readable logging. Returns (false, nil)
// otherwise so the caller proceeds with normal log output.
func MaybeOutputJSON(cmd *cobra.Command, v any) (bool, error) {
	if !WantJSON(cmd) {
		return false, nil
	}
	return true, OutputJSON(v)
}

// OutputFormatted outputs as JSON or table based on --format flag.
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

// digestPullRef pins OCI pulls by digest; returns image as-is for others.
func digestPullRef(image, digest, imageType string) string {
	if digest == "" || imageType != "oci" {
		return image
	}
	// OCI: convert "registry/repo:tag" → "registry/repo@sha256:..."
	ref, err := name.ParseReference(image)
	if err != nil {
		return image
	}
	return ref.Context().String() + "@" + digest
}

func findHypervisorFactory(typ config.HypervisorType) func(*config.Config) (hypervisor.Hypervisor, error) {
	for _, f := range hypervisorFactories {
		if f.typ == typ {
			return f.ctor
		}
	}
	return nil
}

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
		return nil, fmt.Errorf("vm %s: %w", ref, hypervisor.ErrNotFound)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, h := range matches {
			names[i] = h.Type()
		}
		return nil, fmt.Errorf("vm %s: %w (backends: %s)", ref, hypervisor.ErrAmbiguous, strings.Join(names, ", "))
	}
}

// sanitizeVMName derives a safe VM name from an image reference.
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
