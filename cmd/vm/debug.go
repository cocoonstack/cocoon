package vm

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cgroup"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
	"github.com/cocoonstack/cocoon/types"
)

type chDebugSpec struct {
	Configs []*types.StorageConfig
	Boot    *types.BootConfig
	VMCfg   *types.VMConfig
	CowPath string
	CHBin   string
	MaxCPU  int
	Balloon int
	Allowed []int
}

func (h Handler) Debug(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)

	if fc, _ := cmd.Flags().GetBool("fc"); fc {
		conf.UseFirecracker = true
	}

	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}

	vmCfg, err := cmdcore.VMConfigFromFlags(cmd, args[0])
	if err != nil {
		return err
	}
	if err = validateBackendFlags(conf, vmCfg); err != nil {
		return err
	}
	if len(vmCfg.DataDisks) > 0 {
		fmt.Fprintln(os.Stderr, "warning: --data-disk is ignored in debug mode (debug only prints the hypervisor launch command; data disks need PrepareDataDisks to materialize)")
	}

	storageConfigs, boot, err := cmdcore.ResolveImage(ctx, backends, vmCfg)
	if err != nil {
		return err
	}
	if err = validateBootCompat(conf, vmCfg, boot); err != nil {
		return err
	}

	if conf.UseFirecracker {
		// FC requires uncompressed ELF kernel — resolve vmlinux path for debug output.
		if err := firecracker.EnsureVmlinuxBoot(boot); err != nil {
			return err
		}
		printFCDebug(storageConfigs, boot, vmCfg, conf.FCBinary)
		return nil
	}

	printCHDebug(buildCHDebugSpec(cmd, conf, storageConfigs, boot, vmCfg))
	return nil
}

func printFCDebug(configs []*types.StorageConfig, boot *types.BootConfig, vmCfg *types.VMConfig, fcBin string) {
	cowPath := fmt.Sprintf("cow-%s.raw", vmCfg.Name)
	memMiB := int(vmCfg.Memory >> 20) //nolint:mnd

	nLayers := 0
	for _, s := range configs {
		if s.Role == types.StorageRoleLayer {
			nLayers++
		}
	}
	layerDevs := make([]string, nLayers)
	for i := range nLayers {
		layerDevs[nLayers-1-i] = firecracker.DevPath(i)
	}
	cowDev := firecracker.DevPath(nLayers)

	cmdline := hypervisor.BuildBaseCmdline("console=ttyS0 reboot=k loglevel=3 pci=off i8042.noaux 8250.nr_uarts=1",
		strings.Join(layerDevs, ","), cowDev, nil, vmCfg.Name, nil)

	printPrepareCOWDisk(vmCfg.Storage>>30, cowPath) //nolint:mnd

	fmt.Printf("# Launch Firecracker: %s (image: %s)\n", vmCfg.Name, vmCfg.Image)
	fmt.Printf("%s --api-sock /tmp/fc-%s.sock --id %s\n", fcBin, vmCfg.Name, vmCfg.Name)
	fmt.Println()

	fmt.Println("# Configure via REST API (use curl or similar):")
	sock := fmt.Sprintf("/tmp/fc-%s.sock", vmCfg.Name)

	fmt.Println("# 1. Machine config")
	fmt.Printf("curl --unix-socket %s -X PUT http://localhost/machine-config \\\n", sock)
	fmt.Printf("  -d '{\"vcpu_count\": %d, \"mem_size_mib\": %d}'\n", vmCfg.CPU, memMiB)
	fmt.Println()

	fmt.Println("# 2. Boot source")
	fmt.Printf("curl --unix-socket %s -X PUT http://localhost/boot-source \\\n", sock)
	fmt.Printf("  -d '{\"kernel_image_path\": \"%s\", \"initrd_path\": \"%s\", \"boot_args\": \"%s\"}'\n",
		boot.KernelPath, boot.InitrdPath, cmdline)
	fmt.Println()

	fmt.Println("# 3. Drives")
	for i, sc := range configs {
		fmt.Printf("curl --unix-socket %s -X PUT http://localhost/drives/drive_%d \\\n", sock, i)
		fmt.Printf("  -d '{\"drive_id\": \"drive_%d\", \"path_on_host\": \"%s\", \"is_root_device\": false, \"is_read_only\": %t}'\n",
			i, sc.Path, sc.RO)
	}
	fmt.Printf("curl --unix-socket %s -X PUT http://localhost/drives/drive_%d \\\n", sock, len(configs))
	fmt.Printf("  -d '{\"drive_id\": \"drive_%d\", \"path_on_host\": \"%s\", \"is_root_device\": false, \"is_read_only\": false}'\n",
		len(configs), cowPath)
	fmt.Println()

	if size, ok := hypervisor.BalloonSize(vmCfg.Memory, vmCfg.Windows); ok {
		fmt.Println("# 4. Balloon")
		fmt.Printf("curl --unix-socket %s -X PUT http://localhost/balloon \\\n", sock)
		fmt.Printf("  -d '{\"amount_mib\": %d, \"deflate_on_oom\": true, \"free_page_reporting\": true}'\n", size>>20) //nolint:mnd
		fmt.Println()
	}

	fmt.Println("# 5. Start")
	fmt.Printf("curl --unix-socket %s -X PUT http://localhost/actions \\\n", sock)
	fmt.Println("  -d '{\"action_type\": \"InstanceStart\"}'")
}

func buildCHDebugSpec(cmd *cobra.Command, conf *config.Config, storageConfigs []*types.StorageConfig, boot *types.BootConfig, vmCfg *types.VMConfig) chDebugSpec {
	maxCPU, _ := cmd.Flags().GetInt("max-cpu")
	balloon, _ := cmd.Flags().GetInt("balloon")
	cowPath, _ := cmd.Flags().GetString("cow")
	chBin, _ := cmd.Flags().GetString("ch")
	// Mirror runtime gating: Windows / sub-MinBalloon VMs never get balloon even with --balloon, so debug output stays truthful.
	size, ok := hypervisor.BalloonSize(vmCfg.Memory, vmCfg.Windows)
	switch {
	case !ok:
		balloon = 0
	case balloon == 0:
		balloon = int(size >> 20) //nolint:mnd
	}
	allowed := cgroup.EffectiveCPUs(vmCfg.CPUSetCPUs, conf.CgroupCPUs)
	return chDebugSpec{
		Configs: storageConfigs,
		Boot:    boot,
		VMCfg:   vmCfg,
		Allowed: allowed,
		CowPath: cowPath,
		CHBin:   chBin,
		MaxCPU:  maxCPU,
		Balloon: balloon,
	}
}

func printCHDebug(s chDebugSpec) {
	cpu := s.VMCfg.CPU
	diskQueueSize := s.VMCfg.DiskQueueSize
	noDirectIO := s.VMCfg.NoDirectIO

	if hypervisor.IsDirectBoot(s.Boot) {
		if s.CowPath == "" {
			s.CowPath = fmt.Sprintf("cow-%s.raw", s.VMCfg.Name)
		}
		debugConfigs := slices.Concat(s.Configs, []*types.StorageConfig{
			{Path: s.CowPath, RO: false, Serial: hypervisor.CowSerial},
		})
		diskArgs := cloudhypervisor.DebugDiskCLIArgs(debugConfigs, cpu, diskQueueSize, noDirectIO, s.Allowed)
		cocoonLayers := strings.Join(cloudhypervisor.ReverseLayerSerials(s.Configs), ",")
		cmdline := hypervisor.BuildBaseCmdline("console=hvc0 loglevel=3",
			cocoonLayers, hypervisor.CowSerial, nil, s.VMCfg.Name, nil)

		printPrepareCOWDisk(s.VMCfg.Storage>>30, s.CowPath) //nolint:mnd
		fmt.Printf("# Launch VM: %s (image: %s, boot: direct kernel)\n", s.VMCfg.Name, s.VMCfg.Image)
		fmt.Printf("%s \\\n", s.CHBin)
		fmt.Printf("  --kernel %s \\\n", s.Boot.KernelPath)
		fmt.Printf("  --initramfs %s \\\n", s.Boot.InitrdPath)
		fmt.Print("  --disk")
		for _, d := range diskArgs {
			fmt.Printf(" \\\n    \"%s\"", d)
		}
		fmt.Print(" \\\n")
		fmt.Printf("  --cmdline \"%s\" \\\n", cmdline)
	} else {
		if s.CowPath == "" {
			s.CowPath = fmt.Sprintf("cow-%s.qcow2", s.VMCfg.Name)
		}
		basePath := s.Configs[0].Path
		fmt.Println("# Prepare COW overlay")
		fmt.Printf("qemu-img create -f qcow2 -F qcow2 -b %s %s\n", basePath, s.CowPath)
		if s.VMCfg.Storage > 0 {
			fmt.Printf("qemu-img resize %s %dG\n", s.CowPath, s.VMCfg.Storage>>30) //nolint:mnd
		}
		fmt.Println()
		fmt.Printf("# Launch VM: %s (image: %s, boot: UEFI firmware)\n", s.VMCfg.Name, s.VMCfg.Image)
		fmt.Printf("%s \\\n", s.CHBin)
		fmt.Printf("  --firmware %s \\\n", s.Boot.FirmwarePath)
		fmt.Print("  --disk \\\n")
		diskArgs := cloudhypervisor.DebugDiskCLIArgs([]*types.StorageConfig{{Path: s.CowPath, RO: false}}, cpu, diskQueueSize, noDirectIO, s.Allowed)
		fmt.Printf("    \"%s\" \\\n", diskArgs[0])
	}
	printCommonCHArgs(s)
}

func printPrepareCOWDisk(sizeGB int64, path string) {
	fmt.Println("# Prepare COW disk")
	fmt.Printf("truncate -s %dG %s\n", sizeGB, path)
	fmt.Printf("mkfs.ext4 -F -m 0 -q -E lazy_itable_init=1,lazy_journal_init=1,discard %s\n", path)
	fmt.Println()
}

func printCommonCHArgs(s chDebugSpec) {
	cpuExtra := ""
	if s.VMCfg.Windows {
		cpuExtra = ",kvm_hyperv=on"
	}
	memExtra := ""
	if s.VMCfg.HugePages {
		memExtra += ",hugepages=on"
	}
	if s.VMCfg.SharedMemory {
		memExtra += ",shared=on"
	}
	fmt.Printf("  --cpus boot=%d,max=%d%s \\\n", s.VMCfg.CPU, s.MaxCPU, cpuExtra)
	fmt.Printf("  --memory size=%dM%s \\\n", s.VMCfg.Memory>>20, memExtra) //nolint:mnd
	fmt.Print("  --rng src=/dev/urandom \\\n")
	if s.Balloon > 0 {
		fmt.Printf("  --balloon size=%dM,deflate_on_oom=on,free_page_reporting=on \\\n", s.Balloon)
	}
	fmt.Print("  --watchdog \\\n")
	fmt.Println("  --serial tty --console off")
}
