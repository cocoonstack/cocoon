package core

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/docker/go-units"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/types"
)

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
	noWatchdog, _ := cmd.Flags().GetBool("no-watchdog")
	noBalloon, _ := cmd.Flags().GetBool("no-balloon")
	windows, _ := cmd.Flags().GetBool("windows")
	sharedMemory, _ := cmd.Flags().GetBool("shared-memory")
	hugePages, _ := cmd.Flags().GetBool("hugepages")
	mergeable, _ := cmd.Flags().GetBool("mergeable")
	pci, _ := cmd.Flags().GetBool("pci")
	dataDiskRaw, _ := cmd.Flags().GetStringArray("data-disk")

	vmName = cmp.Or(vmName, sanitizeVMName(image))

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
		Name:          vmName,
		CPU:           cpu,
		Memory:        memBytes,
		Storage:       storBytes,
		QueueSize:     queueSize,
		DiskQueueSize: diskQueueSize,
		Image:         image,
		Network:       network,
		NoDirectIO:    noDirectIO,
		NoWatchdog:    noWatchdog,
		NoBalloon:     noBalloon,
		Windows:       windows,
		SharedMemory:  sharedMemory,
		HugePages:     hugePages,
		Mergeable:     mergeable,
		PCI:           pci,
		User:          user,
		Password:      password,
		DataDisks:     dataDisks,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cgroupKnobsFromFlags(cmd, &cfg.Config)
	if err := cgroup.ResolveKnobs(&cfg.Config).Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// CloneVMConfigFromFlags builds VMConfig for a clone. The snapshot's cgroup knobs record its source VM's policy and are never applied; the clone's policy comes from flags alone.
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

	restoreMode, err := restoreModeFromFlags(cmd)
	if err != nil {
		return nil, err
	}
	dataDiskRaw, _ := cmd.Flags().GetStringArray("data-disk")
	dataDisks, err := parseDataDiskFlags(dataDiskRaw)
	if err != nil {
		return nil, err
	}

	cfg := &types.VMConfig{
		Name:          vmName,
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
		NoWatchdog:    snapCfg.NoWatchdog,
		NoBalloon:     snapCfg.NoBalloon,
		Windows:       snapCfg.Windows,
		SharedMemory:  snapCfg.SharedMemory,
		HugePages:     snapCfg.HugePages,
		Mergeable:     snapCfg.Mergeable,
		PCI:           snapCfg.PCI,
		DataDisks:     dataDisks,
		RestoreMode:   restoreMode,
	}
	cgroupKnobsFromFlags(cmd, &cfg.Config)
	if err := cgroup.ResolveKnobs(&cfg.Config).Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// RestoreVMConfigFromFlags builds VMConfig for restore: guest resources from the snapshot; Name, Network, and cgroup knobs from the VM (host-side state survives restore).
func RestoreVMConfigFromFlags(cmd *cobra.Command, vm *types.VM, snapCfg types.SnapshotConfig) (*types.VMConfig, error) {
	if snapCfg.NICs != len(vm.NetworkConfigs) {
		return nil, fmt.Errorf("nic count mismatch: vm has %d, snapshot has %d",
			len(vm.NetworkConfigs), snapCfg.NICs)
	}
	cfg := snapCfg.Config
	cfg.Network = vm.Config.Network
	cfg.CPUWeight = vm.Config.CPUWeight
	cfg.CPUQuotaUs = vm.Config.CPUQuotaUs
	cfg.CPUPeriodUs = vm.Config.CPUPeriodUs
	cfg.CPUBurstUs = vm.Config.CPUBurstUs
	cfg.CPUSetCPUs = vm.Config.CPUSetCPUs
	restoreMode, err := restoreModeFromFlags(cmd)
	if err != nil {
		return nil, err
	}
	result := &types.VMConfig{
		Config:      cfg,
		Name:        vm.Config.Name,
		RestoreMode: restoreMode,
	}
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("snapshot config: %w", err)
	}
	// The kept knobs must fit the snapshot's vCPU count before the destructive phase — a bad combination retries into the same failure.
	if err := cgroup.ResolveKnobs(&result.Config).Validate(); err != nil {
		return nil, err
	}
	return result, nil
}

func EnsureFirmwarePath(conf *config.Config, bootCfg *types.BootConfig) {
	if bootCfg != nil && bootCfg.KernelPath == "" && bootCfg.FirmwarePath == "" {
		bootCfg.FirmwarePath = images.FirmwarePath(conf.RootDir)
	}
}

func sanitizeVMName(image string) string {
	ref, err := name.ParseReference(image)
	if err != nil {
		n := strings.ReplaceAll(image, "/", "-")
		n = strings.ReplaceAll(n, ":", "-")
		n = "cocoon-" + n
		return n[:min(len(n), 63)]
	}

	repo := strings.TrimPrefix(ref.Context().RepositoryStr(), "library/")
	n := "cocoon-" + strings.ReplaceAll(repo, "/", "-")

	// Skip digest — too long for a VM name.
	if tag, ok := ref.(name.Tag); ok && tag.TagStr() != "latest" {
		n += "-" + tag.TagStr()
	}

	return n[:min(len(n), 63)]
}

// parseDataDiskFlags parses --data-disk values, normalizes defaults, and returns the spec list ready for hypervisor.PrepareDataDisks.
func parseDataDiskFlags(raw []string) ([]types.DataDiskSpec, error) {
	specs := make([]types.DataDiskSpec, 0, len(raw))
	for _, s := range raw {
		spec, err := types.ParseDataDiskSpec(s)
		if err != nil {
			return nil, fmt.Errorf("--data-disk: %w", err)
		}
		specs = append(specs, spec)
	}
	if err := normalizeDataDiskSpecs(specs); err != nil {
		return nil, err
	}
	return specs, nil
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

func restoreModeFromFlags(cmd *cobra.Command) (string, error) {
	mode, _ := cmd.Flags().GetString("restore-mode")
	switch mode {
	case "", "copy", "ondemand", "mmap":
		return mode, nil
	default:
		return "", fmt.Errorf("--restore-mode must be copy, ondemand or mmap, got %q", mode)
	}
}

func cgroupKnobsFromFlags(cmd *cobra.Command, cfg *types.Config) {
	cfg.CPUWeight, _ = cmd.Flags().GetInt("cpu-weight")
	cfg.CPUQuotaUs, _ = cmd.Flags().GetInt64("cpu-quota-us")
	cfg.CPUPeriodUs, _ = cmd.Flags().GetInt64("cpu-period-us")
	cfg.CPUBurstUs, _ = cmd.Flags().GetInt64("cpu-burst-us")
	cfg.CPUSetCPUs, _ = cmd.Flags().GetString("cpuset-cpus")
}
