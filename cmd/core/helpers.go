package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

var hypervisorFactories = []hypervisorFactory{
	{config.HypervisorCH, func(ctx context.Context, c *config.Config) (hypervisor.Hypervisor, error) {
		return cloudhypervisor.New(c, MeteringRecorder(ctx, c))
	}},
	{config.HypervisorFirecracker, func(ctx context.Context, c *config.Config) (hypervisor.Hypervisor, error) {
		return firecracker.New(c, MeteringRecorder(ctx, c))
	}},
}

type hypervisorFactory struct {
	typ  config.HypervisorType
	ctor func(context.Context, *config.Config) (hypervisor.Hypervisor, error)
}

type BaseHandler struct {
	ConfProvider func() *config.Config
}

func NewBaseHandler(conf *config.Config) BaseHandler {
	return BaseHandler{ConfProvider: func() *config.Config { return conf }}
}

func (h BaseHandler) Init(cmd *cobra.Command) (context.Context, *config.Config, error) {
	conf, err := h.Conf()
	if err != nil {
		return nil, nil, err
	}
	return CommandContext(cmd), conf, nil
}

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
	hyper, err := InitHypervisor(ctx, conf)
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

func InitHypervisor(ctx context.Context, conf *config.Config) (hypervisor.Hypervisor, error) {
	ctor := findHypervisorFactory(conf.Hypervisor())
	if ctor == nil {
		return nil, fmt.Errorf("unknown hypervisor type: %s", conf.Hypervisor())
	}
	h, err := ctor(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("init hypervisor: %w", err)
	}
	return h, nil
}

func InitAllHypervisors(ctx context.Context, conf *config.Config) ([]hypervisor.Hypervisor, error) {
	result := make([]hypervisor.Hypervisor, 0, len(hypervisorFactories))
	for _, f := range hypervisorFactories {
		h, err := f.ctor(ctx, conf)
		if err != nil {
			return nil, fmt.Errorf("init hypervisor %s: %w", f.typ, err)
		}
		result = append(result, h)
	}
	return result, nil
}

func FindHypervisor(ctx context.Context, conf *config.Config, ref string) (hypervisor.Hypervisor, error) {
	hypers, err := InitAllHypervisors(ctx, conf)
	if err != nil {
		return nil, err
	}
	owner, _, err := resolveVMOwner(ctx, hypers, ref)
	return owner, err
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

// RouteRefs resolves user refs to (hypervisor → full VM IDs).
func RouteRefs(ctx context.Context, hypers []hypervisor.Hypervisor, refs []string) (map[hypervisor.Hypervisor][]string, error) {
	result := map[hypervisor.Hypervisor][]string{}
	for _, ref := range refs {
		owner, vm, err := resolveVMOwner(ctx, hypers, ref)
		if err != nil {
			return nil, err
		}
		result[owner] = append(result[owner], vm.ID)
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

func InitSnapshot(ctx context.Context, conf *config.Config, opts ...localfile.Option) (snapshot.Snapshot, error) {
	s, err := localfile.New(conf, MeteringRecorder(ctx, conf), opts...)
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

// EnsureImage pulls the digest-pinned base if missing; only warns so VerifyBaseFiles surfaces the real error for imported images.
func EnsureImage(ctx context.Context, backends []imagebackend.Images, vmCfg *types.VMConfig) {
	if vmCfg.Image == "" || vmCfg.ImageType == "" {
		return
	}
	logger := log.WithFunc("core.EnsureImage")

	lookupRef := cmp.Or(vmCfg.ImageDigest, vmCfg.Image)

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
		if shapeErr := validateRefShape(pullRef, vmCfg.ImageType); shapeErr != nil {
			logger.Warnf(ctx, "skipping auto-pull of %s: %v — pre-pull manually if missing", pullRef, shapeErr)
			return
		}
		logger.Infof(ctx, "base image not found locally, pulling %s ...", pullRef)
		// Pinned digest with no local hit: force past cloudimg's URL-keyed cache.
		needForce := vmCfg.ImageDigest != ""
		if pullErr := b.Pull(ctx, pullRef, needForce, progress.Nop); pullErr != nil {
			logger.Warnf(ctx, "auto-pull %s failed (imported image?): %v — clone may fail if base layers are missing", pullRef, pullErr)
			return
		}
		// cloudimg has no digest pinning, so a pull may drift; verify the digest after.
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
	return resolveOwner(backends, ref, func(b imagebackend.Images) (bool, error) {
		img, err := b.Inspect(ctx, ref)
		return img != nil, err
	},
		fmt.Errorf("image %q: not found in any backend", ref),
		fmt.Errorf("image %s: %w", ref, imagebackend.ErrAmbiguous),
	)
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
	sharedMemory, _ := cmd.Flags().GetBool("shared-memory")
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
			SharedMemory:  sharedMemory,
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

func CloneVMConfigFromFlags(cmd *cobra.Command, snapCfg types.SnapshotConfig) (*types.VMConfig, error) {
	vmName, _ := cmd.Flags().GetString("name")
	flagNetwork, _ := cmd.Flags().GetString("network")
	network := cmp.Or(flagNetwork, snapCfg.Network)
	flagQueueSize, _ := cmd.Flags().GetInt("queue-size")
	queueSize := cmp.Or(flagQueueSize, snapCfg.QueueSize)
	flagDiskQueueSize, _ := cmd.Flags().GetInt("disk-queue-size")
	diskQueueSize := cmp.Or(flagDiskQueueSize, snapCfg.DiskQueueSize)
	noDirectIO := snapCfg.NoDirectIO
	if cmd.Flags().Changed("no-direct-io") {
		noDirectIO, _ = cmd.Flags().GetBool("no-direct-io")
	}

	onDemand, _ := cmd.Flags().GetBool("on-demand")

	return &types.VMConfig{
		Name: vmName,
		Config: types.Config{
			CPU:           snapCfg.CPU,
			Memory:        snapCfg.Memory,
			Storage:       snapCfg.Storage,
			QueueSize:     queueSize,
			DiskQueueSize: diskQueueSize,
			Image:         snapCfg.Image,
			ImageDigest:   snapCfg.ImageDigest,
			ImageType:     snapCfg.ImageType,
			Network:       network,
			NoDirectIO:    noDirectIO,
			Windows:       snapCfg.Windows,
			SharedMemory:  snapCfg.SharedMemory,
		},
		OnDemand: onDemand,
	}, nil
}

// RestoreVMConfigFromFlags builds VMConfig for restore: resources from the snapshot, Name/Network from the VM (CNI namespace survives restore).
func RestoreVMConfigFromFlags(cmd *cobra.Command, vm *types.VM, snapCfg types.SnapshotConfig) (*types.VMConfig, error) {
	if snapCfg.NICs != len(vm.NetworkConfigs) {
		return nil, fmt.Errorf("nic count mismatch: vm has %d, snapshot has %d",
			len(vm.NetworkConfigs), snapCfg.NICs)
	}
	cfg := snapCfg.Config
	cfg.Network = vm.Config.Network
	onDemand, _ := cmd.Flags().GetBool("on-demand")
	result := &types.VMConfig{
		Config:   cfg,
		Name:     vm.Config.Name,
		OnDemand: onDemand,
	}
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("snapshot config: %w", err)
	}
	return result, nil
}

func EnsureFirmwarePath(conf *config.Config, bootCfg *types.BootConfig) {
	if bootCfg != nil && bootCfg.KernelPath == "" && bootCfg.FirmwarePath == "" {
		bootCfg.FirmwarePath = cloudimg.NewConfig(conf).FirmwarePath()
	}
}

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

func AddOutputFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("output", "o", "", `emit "json" for machine-readable output`)
}

func WantJSON(cmd *cobra.Command) bool {
	out, _ := cmd.Flags().GetString("output")
	return out == "json"
}

// MaybeOutputJSON emits JSON iff --output=json; (true, _) means caller should stop logging.
func MaybeOutputJSON(cmd *cobra.Command, v any) (bool, error) {
	if !WantJSON(cmd) {
		return false, nil
	}
	return true, OutputJSON(v)
}

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

// CloseOnCancel closes c when ctx is canceled; callers `defer CloseOnCancel(ctx, c)()` to stop the watcher on return.
func CloseOnCancel(ctx context.Context, c io.Closer) func() bool {
	return context.AfterFunc(ctx, func() {
		c.Close() //nolint:errcheck,gosec
	})
}

// resolveOwner returns the unique backend where found==true; notFound on zero, ambiguous wrapped on multi-match (lists matched types).
func resolveOwner[T interface{ Type() string }](backends []T, ref string, found func(T) (bool, error), notFound, ambiguous error) (T, error) {
	var matches []T
	var zero T
	for _, b := range backends {
		ok, err := found(b)
		if err != nil {
			return zero, fmt.Errorf("inspect %s in %s: %w", ref, b.Type(), err)
		}
		if ok {
			matches = append(matches, b)
		}
	}
	switch len(matches) {
	case 0:
		return zero, notFound
	case 1:
		return matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, b := range matches {
			names[i] = b.Type()
		}
		return zero, fmt.Errorf("%w (backends: %s)", ambiguous, strings.Join(names, ", "))
	}
}

// validateRefShape rejects URL/OCI ref mismatches early so backends don't surface misleading downstream errors.
func validateRefShape(ref, imageType string) error {
	switch imageType {
	case types.ImageTypeCloudImg:
		if !IsURL(ref) {
			return fmt.Errorf("cloudimg ref %q is not an http(s) URL (imported or bare OCI ref?)", ref)
		}
	case types.ImageTypeOCI:
		if _, err := name.ParseReference(ref); err != nil {
			return fmt.Errorf("oci ref %q is not a valid OCI reference: %w", ref, err)
		}
	}
	return nil
}

// digestPullRef pins OCI pulls by digest; returns image as-is for others.
func digestPullRef(image, digest, imageType string) string {
	if digest == "" || imageType != types.ImageTypeOCI {
		return image
	}
	// OCI: convert "registry/repo:tag" → "registry/repo@sha256:..."
	ref, err := name.ParseReference(image)
	if err != nil {
		return image
	}
	return ref.Context().String() + "@" + digest
}

func findHypervisorFactory(typ config.HypervisorType) func(context.Context, *config.Config) (hypervisor.Hypervisor, error) {
	for _, f := range hypervisorFactories {
		if f.typ == typ {
			return f.ctor
		}
	}
	return nil
}

// resolveVMOwner returns the owning hypervisor and resolved *types.VM so callers use vm.ID instead of re-resolving the raw ref.
func resolveVMOwner(ctx context.Context, hypers []hypervisor.Hypervisor, ref string) (hypervisor.Hypervisor, *types.VM, error) {
	var resolved *types.VM
	owner, err := resolveOwner(hypers, ref, func(h hypervisor.Hypervisor) (bool, error) {
		vm, err := h.Inspect(ctx, ref)
		if err == nil && vm != nil {
			resolved = vm
			return true, nil
		}
		if err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
			return false, err
		}
		return false, nil
	},
		fmt.Errorf("vm %s: %w", ref, hypervisor.ErrNotFound),
		fmt.Errorf("vm %s: %w", ref, hypervisor.ErrAmbiguous),
	)
	return owner, resolved, err
}

func sanitizeVMName(image string) string {
	ref, err := name.ParseReference(image)
	if err != nil {
		n := strings.ReplaceAll(image, "/", "-")
		n = strings.ReplaceAll(n, ":", "-")
		n = "cocoon-" + n
		if len(n) > 63 {
			n = n[:63]
		}
		return n
	}

	repo := strings.TrimPrefix(ref.Context().RepositoryStr(), "library/")
	n := "cocoon-" + strings.ReplaceAll(repo, "/", "-")

	// Skip digest (too long); use tag if not latest.
	if tag, ok := ref.(name.Tag); ok && tag.TagStr() != "latest" {
		n += "-" + tag.TagStr()
	}

	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

// parseDataDiskFlags parses --data-disk values, normalizes defaults, and returns the spec list ready for hypervisor.PrepareDataDisks.
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

// parseDataDiskSpec parses a comma-separated --data-disk arg; size is required (≥16MiB), others default via normalizeDataDiskSpecs.
func parseDataDiskSpec(s string) (types.DataDiskSpec, error) {
	var spec types.DataDiskSpec
	if s == "" {
		return spec, fmt.Errorf("--data-disk: empty spec")
	}
	for part := range strings.SplitSeq(s, ",") {
		rawKey, rawVal, ok := strings.Cut(part, "=")
		if !ok {
			return spec, fmt.Errorf("--data-disk: %q is not key=value", part)
		}
		key := strings.TrimSpace(rawKey)
		val := strings.TrimSpace(rawVal)
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
			if val != types.FSTypeExt4 && val != types.FSTypeNone {
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

// normalizeDataDiskSpecs fills defaults (FSType=ext4, Name=dataN, MountPoint=/mnt/<name>) and enforces unique names; fstype=none rejects non-empty MountPoint.
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
		specs[i].FSType = cmp.Or(specs[i].FSType, types.FSTypeExt4)
		if specs[i].FSType != types.FSTypeExt4 && specs[i].FSType != types.FSTypeNone {
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
		if !specs[i].MountPointSet && specs[i].FSType != types.FSTypeNone {
			specs[i].MountPoint = "/mnt/" + specs[i].Name
		}
		if specs[i].FSType == types.FSTypeNone && specs[i].MountPoint != "" {
			return fmt.Errorf("--data-disk %s: fstype=none requires empty mount", specs[i].Name)
		}
	}
	return nil
}
