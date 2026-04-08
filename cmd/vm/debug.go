package vm

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
	"github.com/cocoonstack/cocoon/types"
)

func (h Handler) Debug(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	// Read --fc from the subcommand flag.
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

	storageConfigs, boot, err := cmdcore.ResolveImage(ctx, backends, vmCfg)
	if err != nil {
		return err
	}

	if conf.UseFirecracker {
		if boot.KernelPath == "" {
			return fmt.Errorf("--fc requires OCI images (direct kernel boot)")
		}
		// FC requires uncompressed ELF kernel — resolve vmlinux path for debug output.
		vmlinuxPath, extractErr := firecracker.EnsureVmlinux(boot.KernelPath)
		if extractErr != nil {
			return fmt.Errorf("extract vmlinux: %w", extractErr)
		}
		boot.KernelPath = vmlinuxPath
		printFCDebug(storageConfigs, boot, vmCfg, conf.FCBinary)
	} else {
		maxCPU, _ := cmd.Flags().GetInt("max-cpu")
		balloon, _ := cmd.Flags().GetInt("balloon")
		cowPath, _ := cmd.Flags().GetString("cow")
		chBin, _ := cmd.Flags().GetString("ch")
		memoryMB := int(vmCfg.Memory >> 20)   //nolint:mnd
		cowSizeGB := int(vmCfg.Storage >> 30) //nolint:mnd
		if balloon == 0 {
			balloon = memoryMB / 2 //nolint:mnd
		}
		printCHDebug(storageConfigs, boot, vmCfg.Name, vmCfg.Image, cowPath, chBin, vmCfg.CPU, maxCPU, memoryMB, balloon, cowSizeGB, vmCfg.Windows, boot.KernelPath != "")
	}
	return nil
}

// printFCDebug outputs the Firecracker launch sequence as shell commands.
func printFCDebug(configs []*types.StorageConfig, boot *types.BootConfig, vmCfg *types.VMConfig, fcBin string) {
	cowPath := fmt.Sprintf("cow-%s.raw", vmCfg.Name)
	memMiB := int(vmCfg.Memory >> 20)     //nolint:mnd
	cowSizeGB := int(vmCfg.Storage >> 30) //nolint:mnd

	// Build layer device paths (reversed for overlayfs)
	nLayers := 0
	for _, s := range configs {
		if s.RO {
			nLayers++
		}
	}
	layerDevs := make([]string, nLayers)
	for i := range nLayers {
		layerDevs[nLayers-1-i] = fmt.Sprintf("/dev/vd%c", 'a'+i)
	}
	cowDev := fmt.Sprintf("/dev/vd%c", 'a'+nLayers)

	cmdline := fmt.Sprintf(
		"console=ttyS0 reboot=k loglevel=3 boot=cocoon-overlay cocoon.layers=%s cocoon.cow=%s clocksource=kvm-clock rw",
		strings.Join(layerDevs, ","), cowDev)

	fmt.Println("# Prepare COW disk")
	fmt.Printf("truncate -s %dG %s\n", cowSizeGB, cowPath)
	fmt.Printf("mkfs.ext4 -F -m 0 -q -E lazy_itable_init=1,lazy_journal_init=1,discard %s\n", cowPath)
	fmt.Println()

	fmt.Printf("# Launch Firecracker: %s (image: %s)\n", vmCfg.Name, vmCfg.Image)
	fmt.Printf("%s --api-sock /tmp/fc-%s.sock --id %s\n", fcBin, vmCfg.Name, vmCfg.Name)
	fmt.Println()

	fmt.Println("# Configure via REST API (use curl or similar):")
	sock := fmt.Sprintf("/tmp/fc-%s.sock", vmCfg.Name)

	fmt.Printf("# 1. Machine config\n")
	fmt.Printf("curl --unix-socket %s -X PUT http://localhost/machine-config \\\n", sock)
	fmt.Printf("  -d '{\"vcpu_count\": %d, \"mem_size_mib\": %d}'\n", vmCfg.CPU, memMiB)
	fmt.Println()

	fmt.Printf("# 2. Boot source\n")
	fmt.Printf("curl --unix-socket %s -X PUT http://localhost/boot-source \\\n", sock)
	fmt.Printf("  -d '{\"kernel_image_path\": \"%s\", \"initrd_path\": \"%s\", \"boot_args\": \"%s\"}'\n",
		boot.KernelPath, boot.InitrdPath, cmdline)
	fmt.Println()

	fmt.Printf("# 3. Drives\n")
	for i, sc := range configs {
		fmt.Printf("curl --unix-socket %s -X PUT http://localhost/drives/drive_%d \\\n", sock, i)
		fmt.Printf("  -d '{\"drive_id\": \"drive_%d\", \"path_on_host\": \"%s\", \"is_root_device\": false, \"is_read_only\": %t}'\n",
			i, sc.Path, sc.RO)
	}
	// COW drive
	fmt.Printf("curl --unix-socket %s -X PUT http://localhost/drives/drive_%d \\\n", sock, len(configs))
	fmt.Printf("  -d '{\"drive_id\": \"drive_%d\", \"path_on_host\": \"%s\", \"is_root_device\": false, \"is_read_only\": false}'\n",
		len(configs), cowPath)
	fmt.Println()

	balloonMiB := memMiB / 4     //nolint:mnd
	if vmCfg.Memory >= 256<<20 { //nolint:mnd
		fmt.Printf("# 4. Balloon\n")
		fmt.Printf("curl --unix-socket %s -X PUT http://localhost/balloon \\\n", sock)
		fmt.Printf("  -d '{\"amount_mib\": %d, \"deflate_on_oom\": true, \"free_page_reporting\": true}'\n", balloonMiB)
		fmt.Println()
	}

	fmt.Printf("# 5. Start\n")
	fmt.Printf("curl --unix-socket %s -X PUT http://localhost/actions \\\n", sock)
	fmt.Printf("  -d '{\"action_type\": \"InstanceStart\"}'\n")
}

func printCHDebug(configs []*types.StorageConfig, boot *types.BootConfig, vmName, image, cowPath, chBin string, cpu, maxCPU, memory, balloon, cowSize int, windows, directBoot bool) {
	if directBoot {
		if cowPath == "" {
			cowPath = fmt.Sprintf("cow-%s.raw", vmName)
		}
		debugConfigs := append(append([]*types.StorageConfig(nil), configs...),
			&types.StorageConfig{Path: cowPath, RO: false, Serial: hypervisor.CowSerial})
		diskArgs := cloudhypervisor.DebugDiskCLIArgs(debugConfigs, cpu)
		cocoonLayers := strings.Join(cloudhypervisor.ReverseLayerSerials(configs), ",")
		cmdline := fmt.Sprintf(
			"console=hvc0 loglevel=3 boot=cocoon-overlay cocoon.layers=%s cocoon.cow=%s clocksource=kvm-clock rw",
			cocoonLayers, hypervisor.CowSerial)

		fmt.Println("# Prepare COW disk")
		fmt.Printf("truncate -s %dG %s\n", cowSize, cowPath)
		fmt.Printf("mkfs.ext4 -F -m 0 -q -E lazy_itable_init=1,lazy_journal_init=1,discard %s\n", cowPath)
		fmt.Println()
		fmt.Printf("# Launch VM: %s (image: %s, boot: direct kernel)\n", vmName, image)
		fmt.Printf("%s \\\n", chBin)
		fmt.Printf("  --kernel %s \\\n", boot.KernelPath)
		fmt.Printf("  --initramfs %s \\\n", boot.InitrdPath)
		fmt.Printf("  --disk")
		for _, d := range diskArgs {
			fmt.Printf(" \\\n    \"%s\"", d)
		}
		fmt.Printf(" \\\n")
		fmt.Printf("  --cmdline \"%s\" \\\n", cmdline)
	} else {
		if cowPath == "" {
			cowPath = fmt.Sprintf("cow-%s.qcow2", vmName)
		}
		basePath := configs[0].Path
		fmt.Println("# Prepare COW overlay")
		fmt.Printf("qemu-img create -f qcow2 -F qcow2 -b %s %s\n", basePath, cowPath)
		if cowSize > 0 {
			fmt.Printf("qemu-img resize %s %dG\n", cowPath, cowSize)
		}
		fmt.Println()
		fmt.Printf("# Launch VM: %s (image: %s, boot: UEFI firmware)\n", vmName, image)
		fmt.Printf("%s \\\n", chBin)
		fmt.Printf("  --firmware %s \\\n", boot.FirmwarePath)
		fmt.Printf("  --disk \\\n")
		diskArgs := cloudhypervisor.DebugDiskCLIArgs([]*types.StorageConfig{{Path: cowPath, RO: false}}, cpu)
		fmt.Printf("    \"%s\" \\\n", diskArgs[0])
	}
	printCommonCHArgs(cpu, maxCPU, memory, balloon, windows)
}

func printCommonCHArgs(cpu, maxCPU, memory, balloon int, windows bool) {
	cpuExtra := ""
	if windows {
		cpuExtra = ",kvm_hyperv=on"
	}
	fmt.Printf("  --cpus boot=%d,max=%d%s \\\n", cpu, maxCPU, cpuExtra)
	fmt.Printf("  --memory size=%dM \\\n", memory)
	fmt.Printf("  --rng src=/dev/urandom \\\n")
	fmt.Printf("  --balloon size=%dM,deflate_on_oom=on,free_page_reporting=on \\\n", balloon)
	fmt.Printf("  --watchdog \\\n")
	fmt.Printf("  --serial tty --console off\n")
}
