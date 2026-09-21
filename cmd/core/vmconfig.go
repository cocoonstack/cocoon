package core

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/cmd/cliutil"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/types"
)

func VMConfigFromFlags(cmd *cobra.Command, image string) (*types.VMConfig, error) {
	memStr, storStr := cliutil.FlagStr(cmd, "memory"), cliutil.FlagStr(cmd, "storage")
	memBytes, err := types.ParseSize(memStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --memory %q: %w", memStr, err)
	}
	storBytes, err := types.ParseSize(storStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --storage %q: %w", storStr, err)
	}

	dataDisks, err := parseDataDiskFlags(cliutil.FlagStrings(cmd, "data-disk"))
	if err != nil {
		return nil, err
	}

	cfg := &types.VMConfig{
		Name:          cmp.Or(cliutil.FlagStr(cmd, "name"), sanitizeVMName(image)),
		CPU:           cliutil.FlagInt(cmd, "cpu"),
		Memory:        memBytes,
		Storage:       storBytes,
		QueueSize:     cliutil.FlagInt(cmd, "queue-size"),
		DiskQueueSize: cliutil.FlagInt(cmd, "disk-queue-size"),
		Image:         image,
		Network:       cliutil.FlagStr(cmd, "network"),
		NoDirectIO:    cliutil.FlagBool(cmd, "no-direct-io"),
		NoWatchdog:    cliutil.FlagBool(cmd, "no-watchdog"),
		NoBalloon:     cliutil.FlagBool(cmd, "no-balloon"),
		Windows:       cliutil.FlagBool(cmd, "windows"),
		SharedMemory:  cliutil.FlagBool(cmd, "shared-memory"),
		HugePages:     cliutil.FlagBool(cmd, "hugepages"),
		Mergeable:     cliutil.FlagBool(cmd, "mergeable"),
		PCI:           cliutil.FlagBool(cmd, "pci"),
		User:          cliutil.FlagStr(cmd, "user"),
		Password:      cliutil.FlagStr(cmd, "password"),
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
	noDirectIO := snapCfg.NoDirectIO
	if cmd.Flags().Changed("no-direct-io") {
		noDirectIO = cliutil.FlagBool(cmd, "no-direct-io")
	}

	restoreMode, err := restoreModeFromFlags(cmd)
	if err != nil {
		return nil, err
	}
	dataDisks, err := parseDataDiskFlags(cliutil.FlagStrings(cmd, "data-disk"))
	if err != nil {
		return nil, err
	}

	cfg := &types.VMConfig{
		Name:          cliutil.FlagStr(cmd, "name"),
		CPU:           snapCfg.CPU,
		Memory:        snapCfg.Memory,
		Storage:       snapCfg.Storage,
		QueueSize:     cmp.Or(cliutil.FlagInt(cmd, "queue-size"), snapCfg.QueueSize),
		DiskQueueSize: cmp.Or(cliutil.FlagInt(cmd, "disk-queue-size"), snapCfg.DiskQueueSize),
		Image:         snapCfg.Image,
		ImageDigest:   snapCfg.ImageDigest,
		ImageType:     snapCfg.ImageType,
		Network:       cmp.Or(cliutil.FlagStr(cmd, "network"), snapCfg.Network),
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
	switch mode := cliutil.FlagStr(cmd, "restore-mode"); mode {
	case "", "copy", "ondemand", "mmap":
		return mode, nil
	default:
		return "", fmt.Errorf("--restore-mode must be copy, ondemand or mmap, got %q", mode)
	}
}

func cgroupKnobsFromFlags(cmd *cobra.Command, cfg *types.Config) {
	cfg.CPUWeight = cliutil.FlagInt(cmd, "cpu-weight")
	cfg.CPUQuotaUs = cliutil.FlagInt64(cmd, "cpu-quota-us")
	cfg.CPUPeriodUs = cliutil.FlagInt64(cmd, "cpu-period-us")
	cfg.CPUBurstUs = cliutil.FlagInt64(cmd, "cpu-burst-us")
	cfg.CPUSetCPUs = cliutil.FlagStr(cmd, "cpuset-cpus")
}
