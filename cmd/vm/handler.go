package vm

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	units "github.com/docker/go-units"
	"github.com/moby/term"
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/projecteru2/cocoon/cmd/core"
	"github.com/projecteru2/cocoon/console"
	"github.com/projecteru2/cocoon/hypervisor/cloudhypervisor"
	"github.com/projecteru2/cocoon/service"
	"github.com/projecteru2/cocoon/types"
)

type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Create(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	params, err := cmdcore.VMCreateParamsFromFlags(cmd, args[0])
	if err != nil {
		return err
	}

	vm, err := svc.CreateVM(ctx, params)
	if err != nil {
		return err
	}

	logger := log.WithFunc("cmd.create")
	logger.Infof(ctx, "VM created: %s (name: %s, state: %s)", vm.ID, vm.Config.Name, vm.State)
	logger.Infof(ctx, "start with: cocoon vm start %s", vm.ID)
	return nil
}

func (h Handler) Run(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	params, err := cmdcore.VMCreateParamsFromFlags(cmd, args[0])
	if err != nil {
		return err
	}

	vm, err := svc.RunVM(ctx, params)
	if err != nil {
		return err
	}

	logger := log.WithFunc("cmd.run")
	logger.Infof(ctx, "VM created: %s (name: %s)", vm.ID, vm.Config.Name)
	logger.Infof(ctx, "started: %s", vm.ID)
	return nil
}

func (h Handler) Clone(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	params, err := cmdcore.VMCloneParamsFromFlags(cmd, args[0])
	if err != nil {
		return err
	}

	logger := log.WithFunc("cmd.clone")
	logger.Infof(ctx, "cloning VM from snapshot %s ...", args[0])

	vm, networkConfigs, err := svc.CloneVM(ctx, params)
	if err != nil {
		return err
	}

	logger.Infof(ctx, "VM cloned: %s (name: %s)", vm.ID, vm.Config.Name)
	printPostCloneHints(vm, networkConfigs)
	return nil
}

func (h Handler) Start(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	return batchVMCmd(ctx, "start", "started", func(refs []string) ([]string, error) {
		return svc.StartVM(ctx, refs)
	}, args)
}

func (h Handler) Stop(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	return batchVMCmd(ctx, "stop", "stopped", func(refs []string) ([]string, error) {
		return svc.StopVM(ctx, refs)
	}, args)
}

func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	vms, err := svc.ListVM(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	if len(vms) == 0 {
		fmt.Println("No VMs found.")
		return nil
	}

	slices.SortFunc(vms, func(a, b *types.VM) int { return a.CreatedAt.Compare(b.CreatedAt) })

	return cmdcore.OutputFormatted(cmd, vms, func(w *tabwriter.Writer) {
		fmt.Fprintln(w, "ID\tNAME\tSTATE\tCPU\tMEMORY\tSTORAGE\tIP\tIMAGE\tCREATED") //nolint:errcheck
		for _, vm := range vms {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
				vm.ID, vm.Config.Name, vm.State,
				vm.Config.CPU, units.BytesSize(float64(vm.Config.Memory)),
				units.BytesSize(float64(vm.Config.Storage)),
				vmIPs(vm), vm.Config.Image,
				vm.CreatedAt.Local().Format(time.DateTime))
		}
	})
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	info, err := svc.InspectVM(ctx, args[0])
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}

	return cmdcore.OutputJSON(info)
}

func (h Handler) Console(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	ref := args[0]

	conn, err := svc.ConsoleVM(ctx, ref)
	if err != nil {
		return fmt.Errorf("console: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	escapeStr, _ := cmd.Flags().GetString("escape-char")
	escapeChar, err := console.ParseEscapeChar(escapeStr)
	if err != nil {
		return err
	}

	inFd := os.Stdin.Fd()
	if !term.IsTerminal(inFd) {
		return fmt.Errorf("stdin is not a terminal")
	}

	oldState, err := term.SetRawTerminal(inFd)
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer func() {
		_ = term.RestoreTerminal(inFd, oldState)
		fmt.Fprintf(os.Stderr, "\r\nDisconnected from %s.\r\n", ref)
	}()

	escapeDisplay := console.FormatEscapeChar(escapeChar)
	fmt.Fprintf(os.Stderr, "Connected to %s (escape sequence: %s.)\r\n", ref, escapeDisplay)

	rw, ok := conn.(io.ReadWriter)
	if !ok {
		return fmt.Errorf("console connection does not support writing")
	}

	// Propagate terminal resize to PTY-backed consoles (direct boot / OCI).
	if f, ok := conn.(*os.File); ok {
		cleanup := console.HandleResize(inFd, f.Fd())
		defer cleanup()
	}

	escapeKeys := []byte{escapeChar, '.'}
	if err := console.Relay(rw, escapeKeys); err != nil {
		return fmt.Errorf("relay: %w", err)
	}

	return nil
}

func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	force, _ := cmd.Flags().GetBool("force")
	logger := log.WithFunc("cmd.rm")

	deleted, deleteErr := svc.RemoveVM(ctx, &service.VMRMParams{Refs: args, Force: force})
	for _, id := range deleted {
		logger.Infof(ctx, "deleted VM: %s", id)
	}

	if deleteErr != nil {
		return deleteErr
	}

	if len(deleted) == 0 {
		logger.Info(ctx, "no VMs deleted")
	}

	return nil
}

func (h Handler) Restore(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	params, err := cmdcore.VMRestoreParamsFromFlags(cmd, args[0], args[1])
	if err != nil {
		return err
	}

	logger := log.WithFunc("cmd.restore")
	logger.Infof(ctx, "restoring VM %s from snapshot %s ...", args[0], args[1])

	result, err := svc.RestoreVM(ctx, params)
	if err != nil {
		return err
	}

	logger.Infof(ctx, "VM %s restored (state: %s)", result.ID, result.State)
	return nil
}

func (h Handler) Debug(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	params, err := cmdcore.DebugParamsFromFlags(cmd, args[0])
	if err != nil {
		return err
	}

	info, err := svc.DebugVM(ctx, params)
	if err != nil {
		return err
	}

	memoryMB := int(params.Memory >> 20)  //nolint:mnd
	cowSizeGB := int(params.Storage >> 30) //nolint:mnd
	balloon := params.Balloon
	if balloon == 0 {
		balloon = memoryMB / 2 //nolint:mnd
	}

	if info.BootConfig.KernelPath != "" {
		printRunOCI(info.StorageConfigs, info.BootConfig, params.Name, params.Image, params.COWPath, params.CHBin, params.CPU, params.MaxCPU, memoryMB, balloon, cowSizeGB)
	} else {
		printRunCloudimg(info.StorageConfigs, info.BootConfig, params.Name, params.Image, params.COWPath, params.CHBin, params.CPU, params.MaxCPU, memoryMB, balloon, cowSizeGB)
	}

	return nil
}

// --- helpers (presentation only) ---

func batchVMCmd(ctx context.Context, name, pastTense string, fn func([]string) ([]string, error), refs []string) error {
	logger := log.WithFunc("cmd." + name)

	done, err := fn(refs)
	for _, id := range done {
		logger.Infof(ctx, "%s: %s", pastTense, id)
	}

	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	if len(done) == 0 {
		logger.Infof(ctx, "no VMs %s", strings.ToLower(pastTense))
	}

	return nil
}

// printPostCloneHints outputs commands the user should run inside the guest
// after a clone to reconfigure network and release balloon memory.
func printPostCloneHints(vm *types.VM, networkConfigs []*types.NetworkConfig) {
	isCloudimg := slices.ContainsFunc(vm.StorageConfigs, func(sc *types.StorageConfig) bool {
		return strings.HasSuffix(sc.Path, ".qcow2")
	})

	fmt.Println()
	fmt.Println("Run inside the guest to finish setup:")
	fmt.Println()
	fmt.Println("  # Release memory for balloon")
	fmt.Println("  echo 3 > /proc/sys/vm/drop_caches")

	if isCloudimg {
		printCloudimgNetworkHints(networkConfigs)
	} else {
		printOCINetworkHints(vm, networkConfigs)
	}

	fmt.Println()
}

func printCloudimgNetworkHints(_ []*types.NetworkConfig) {
	fmt.Println()
	fmt.Println("  # Clean old network configs from snapshot and reconfigure via cloud-init")
	fmt.Println("  rm -f /etc/systemd/network/10-*.network")
	fmt.Println("  cloud-init clean --logs --seed --configs network && cloud-init init --local && cloud-init init")
	fmt.Println("  cloud-init modules --mode=config && systemctl restart systemd-networkd")
}

type nicHint struct {
	mac, ip, gw string
	prefix      int
}

func printOCINetworkHints(vm *types.VM, networkConfigs []*types.NetworkConfig) {
	fmt.Println()
	fmt.Printf("  # Set hostname\n")
	fmt.Printf("  hostnamectl set-hostname %s\n", vm.Config.Name)

	var staticNICs []nicHint
	var dhcpMACs []string

	for _, nc := range networkConfigs {
		if nc == nil || nc.Mac == "" {
			continue
		}

		if nc.Network != nil && nc.Network.IP != "" {
			staticNICs = append(staticNICs, nicHint{
				mac:    nc.Mac,
				ip:     nc.Network.IP,
				prefix: nc.Network.Prefix,
				gw:     nc.Network.Gateway,
			})
		} else {
			dhcpMACs = append(dhcpMACs, nc.Mac)
		}
	}

	if len(staticNICs) == 0 && len(dhcpMACs) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("  # Clean old network configs from snapshot and write new ones (MAC-based)")
	fmt.Println("  rm -f /etc/systemd/network/10-*.network")

	if len(staticNICs) > 0 {
		printBashArray("macs", staticNICs, func(n nicHint) string { return n.mac })
		printBashArray("addrs", staticNICs, func(n nicHint) string { return fmt.Sprintf("%s/%d", n.ip, n.prefix) })

		hasGW := slices.ContainsFunc(staticNICs, func(n nicHint) bool { return n.gw != "" })
		if hasGW {
			printBashArray("gws", staticNICs, func(n nicHint) string { return n.gw })
		}

		fmt.Println("  for i in \"${!macs[@]}\"; do")
		fmt.Println("    f=\"/etc/systemd/network/10-${macs[$i]//:/}.network\"")
		writeNet := `    printf '[Match]\nMACAddress=` + `%s\n\n[Network]\nAddress=%s\n' "${macs[$i]}" "${addrs[$i]}" > "$f"`
		fmt.Println(writeNet)

		if hasGW {
			writeGW := `    [ -n "${gws[$i]}" ] && printf 'Gateway=` + `%s\n' "${gws[$i]}" >> "$f"`
			fmt.Println(writeGW)
		}

		fmt.Println("  done")
	}

	if len(dhcpMACs) > 0 {
		fmt.Println("  # DHCP NICs")
		for _, mac := range dhcpMACs {
			sanitized := strings.ReplaceAll(mac, ":", "")
			writeDHCP := fmt.Sprintf(`  printf '[Match]\nMACAddress=%s\n\n[Network]\nDHCP=ipv4\n'`+` > "/etc/systemd/network/10-%s.network"`, mac, sanitized)
			fmt.Println(writeDHCP)
		}
	}

	fmt.Println("  systemctl restart systemd-networkd")
}

func printBashArray(name string, nics []nicHint, field func(nicHint) string) {
	fmt.Printf("  %s=(", name)
	for i, n := range nics {
		if i > 0 {
			fmt.Print(" ")
		}
		fmt.Printf("'%s'", field(n))
	}
	fmt.Println(")")
}

func printRunOCI(configs []*types.StorageConfig, boot *types.BootConfig, vmName, image, cowPath, chBin string, cpu, maxCPU, memory, balloon, cowSize int) {
	if cowPath == "" {
		cowPath = fmt.Sprintf("cow-%s.raw", vmName)
	}

	debugConfigs := append(append([]*types.StorageConfig(nil), configs...),
		&types.StorageConfig{Path: cowPath, RO: false, Serial: cloudhypervisor.CowSerial})
	diskArgs := cloudhypervisor.DebugDiskCLIArgs(debugConfigs, cpu)
	cocoonLayers := strings.Join(cloudhypervisor.ReverseLayerSerials(configs), ",")

	cmdline := fmt.Sprintf(
		"console=hvc0 loglevel=3 boot=cocoon-overlay cocoon.layers=%s cocoon.cow=%s clocksource=kvm-clock rw",
		cocoonLayers, cloudhypervisor.CowSerial)

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
	printCommonCHArgs(cpu, maxCPU, memory, balloon)
}

func printRunCloudimg(configs []*types.StorageConfig, boot *types.BootConfig, vmName, image, cowPath, chBin string, cpu, maxCPU, memory, balloon, cowSize int) {
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
	printCommonCHArgs(cpu, maxCPU, memory, balloon)
}

func vmIPs(vm *types.VM) string {
	var ips []string
	for _, nc := range vm.NetworkConfigs {
		if nc != nil && nc.Network != nil && nc.Network.IP != "" {
			ips = append(ips, nc.Network.IP)
		}
	}

	if len(ips) == 0 {
		return "-"
	}

	return strings.Join(ips, ",")
}

func printCommonCHArgs(cpu, maxCPU, memory, balloon int) {
	fmt.Printf("  --cpus boot=%d,max=%d \\\n", cpu, maxCPU)
	fmt.Printf("  --memory size=%dM \\\n", memory)
	fmt.Printf("  --rng src=/dev/urandom \\\n")
	fmt.Printf("  --balloon size=%dM,deflate_on_oom=on,free_page_reporting=on \\\n", balloon)
	fmt.Printf("  --watchdog \\\n")
	fmt.Printf("  --serial tty --console off\n")
}
