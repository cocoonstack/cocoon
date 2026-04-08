package vm

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (h Handler) Create(cmd *cobra.Command, args []string) error {
	ctx, vm, _, err := h.createVM(cmd, args[0])
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.create")
	logger.Infof(ctx, "VM created: %s (name: %s, state: %s)", vm.ID, vm.Config.Name, vm.State)
	logger.Infof(ctx, "start with: cocoon vm start %s", vm.ID)
	return nil
}

func (h Handler) Run(cmd *cobra.Command, args []string) error {
	ctx, vm, hyper, err := h.createVM(cmd, args[0])
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.run")
	logger.Infof(ctx, "VM created: %s (name: %s)", vm.ID, vm.Config.Name)

	started, err := hyper.Start(ctx, []string{vm.ID})
	if err != nil {
		return fmt.Errorf("start VM %s: %w", vm.ID, err)
	}
	for _, id := range started {
		logger.Infof(ctx, "started: %s", id)
	}
	return nil
}

func (h Handler) Clone(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.clone")

	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	snapRef := args[0]

	// Infer hypervisor backend from the snapshot's Hypervisor field.
	snapInfo, err := snapBackend.Inspect(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("inspect snapshot %s: %w", snapRef, err)
	}
	if snapInfo.Hypervisor != "" {
		conf.UseFirecracker = snapInfo.Hypervisor == string(config.HypervisorFirecracker)
	}

	hyper, err := cmdcore.InitHypervisor(conf)
	if err != nil {
		return err
	}

	if da, ok := snapBackend.(snapshot.Direct); ok {
		if dcr, ok := hyper.(hypervisor.Direct); ok {
			return h.cloneDirect(ctx, cmd, conf, dcr, da, snapRef, logger)
		}
	}

	cfg, stream, err := snapBackend.Restore(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}
	defer stream.Close() //nolint:errcheck

	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	vmCfg, vmID, netProvider, networkConfigs, err := h.prepareClone(ctx, cmd, conf, cfg)
	if err != nil {
		return err
	}

	logger.Infof(ctx, "cloning VM from snapshot %s ...", snapRef)

	vm, cloneErr := hyper.Clone(ctx, vmID, vmCfg, networkConfigs, cfg, stream)
	if cloneErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return fmt.Errorf("clone VM: %w", cloneErr)
	}

	logger.Infof(ctx, "VM cloned: %s (name: %s)", vm.ID, vm.Config.Name)
	printPostCloneHints(vm, networkConfigs)
	return nil
}

func (h Handler) cloneDirect(ctx context.Context, cmd *cobra.Command, conf *config.Config, dcr hypervisor.Direct, da snapshot.Direct, snapRef string, logger *log.Fields) error {
	dataDir, cfg, err := da.DataDir(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}

	vmCfg, vmID, netProvider, networkConfigs, err := h.prepareClone(ctx, cmd, conf, cfg)
	if err != nil {
		return err
	}

	logger.Infof(ctx, "cloning VM from snapshot %s (direct) ...", snapRef)

	vm, cloneErr := dcr.DirectClone(ctx, vmID, vmCfg, networkConfigs, cfg, dataDir)
	if cloneErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return fmt.Errorf("clone VM: %w", cloneErr)
	}

	logger.Infof(ctx, "VM cloned: %s (name: %s)", vm.ID, vm.Config.Name)
	printPostCloneHints(vm, networkConfigs)
	return nil
}

func (h Handler) prepareClone(ctx context.Context, cmd *cobra.Command, conf *config.Config, cfg *types.SnapshotConfig) (*types.VMConfig, string, network.Network, []*types.NetworkConfig, error) {
	vmCfg, err := cmdcore.CloneVMConfigFromFlags(cmd, cfg)
	if err != nil {
		return nil, "", nil, nil, err
	}
	vmID, err := utils.GenerateID()
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("generate VM ID: %w", err)
	}
	if vmCfg.Name == "" {
		vmCfg.Name = "cocoon-clone-" + vmID[:8]
	}

	// FC snapshot/load cannot change CPU, memory, or NIC count.
	// Reject overrides early before creating network/dirs.
	if conf.UseFirecracker {
		if validateErr := validateFCCloneOverrides(cmd, cfg); validateErr != nil {
			return nil, "", nil, nil, validateErr
		}
	}

	nics, _ := cmd.Flags().GetInt("nics")
	if nics == 0 {
		nics = cfg.NICs
	}
	if nics < cfg.NICs {
		return nil, "", nil, nil, fmt.Errorf("--nics %d below snapshot minimum %d", nics, cfg.NICs)
	}

	netProvider, networkConfigs, err := initNetwork(ctx, conf, vmID, nics, vmCfg, tapQueues(vmCfg.CPU, conf.UseFirecracker))
	if err != nil {
		return nil, "", nil, nil, err
	}

	return vmCfg, vmID, netProvider, networkConfigs, nil
}

func (h Handler) restoreDirect(ctx context.Context, snapRef, vmRef string, vmCfg *types.VMConfig, snapBackend snapshot.Snapshot, hyper hypervisor.Hypervisor, logger *log.Fields) (bool, error) {
	da, ok := snapBackend.(snapshot.Direct)
	if !ok {
		return false, nil
	}
	dcr, ok := hyper.(hypervisor.Direct)
	if !ok {
		return false, nil
	}

	dataDir, _, err := da.DataDir(ctx, snapRef)
	if err != nil {
		return true, fmt.Errorf("open snapshot: %w", err)
	}

	logger.Infof(ctx, "restoring VM %s from snapshot %s (direct) ...", vmRef, snapRef)
	result, err := dcr.DirectRestore(ctx, vmRef, vmCfg, dataDir)
	if err != nil {
		return true, fmt.Errorf("restore: %w", err)
	}

	logger.Infof(ctx, "VM %s restored (state: %s)", result.ID, result.State)
	return true, nil
}

func (h Handler) Restore(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.restore")

	vmRef := args[0]
	snapRef := args[1]

	hyper, err := cmdcore.FindHypervisor(ctx, conf, vmRef)
	if err != nil {
		return fmt.Errorf("find VM %s: %w", vmRef, err)
	}
	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	vm, err := hyper.Inspect(ctx, vmRef)
	if err != nil {
		return fmt.Errorf("inspect VM: %w", err)
	}
	snapInfo, err := snapBackend.Inspect(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("inspect snapshot: %w", err)
	}
	if _, ok := vm.SnapshotIDs[snapInfo.ID]; !ok {
		return fmt.Errorf("snapshot %s does not belong to VM %s", snapRef, vmRef)
	}

	if snapInfo.NICs != len(vm.NetworkConfigs) {
		return fmt.Errorf("nic count mismatch: vm has %d, snapshot has %d",
			len(vm.NetworkConfigs), snapInfo.NICs)
	}

	vmCfg, err := cmdcore.RestoreVMConfigFromFlags(cmd, vm, &snapInfo.SnapshotConfig)
	if err != nil {
		return err
	}

	done, directErr := h.restoreDirect(ctx, snapRef, vmRef, vmCfg, snapBackend, hyper, logger)
	if done {
		return directErr
	}

	_, stream, err := snapBackend.Restore(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer stream.Close() //nolint:errcheck

	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	logger.Infof(ctx, "restoring VM %s from snapshot %s ...", vmRef, snapRef)

	result, err := hyper.Restore(ctx, vmRef, vmCfg, stream)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	logger.Infof(ctx, "VM %s restored (state: %s)", result.ID, result.State)
	return nil
}

func (h Handler) createVM(cmd *cobra.Command, image string) (context.Context, *types.VM, hypervisor.Hypervisor, error) {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	// Read --fc from the subcommand flag (create/run/debug only).
	if fc, _ := cmd.Flags().GetBool("fc"); fc {
		conf.UseFirecracker = true
	}

	vmCfg, err := cmdcore.VMConfigFromFlags(cmd, image)
	if err != nil {
		return nil, nil, nil, err
	}

	// Validate backend/boot-mode constraints before initializing backends.
	if conf.UseFirecracker && vmCfg.Windows {
		return nil, nil, nil, fmt.Errorf("--fc and --windows are mutually exclusive: Firecracker does not support Windows guests")
	}

	backends, hyper, err := cmdcore.InitBackends(ctx, conf)
	if err != nil {
		return nil, nil, nil, err
	}

	storageConfigs, bootCfg, err := cmdcore.ResolveImage(ctx, backends, vmCfg)
	if err != nil {
		return nil, nil, nil, err
	}
	if vmCfg.Windows && bootCfg.KernelPath != "" {
		return nil, nil, nil, fmt.Errorf("--windows requires cloudimg (UEFI boot), got OCI direct boot image")
	}
	if conf.UseFirecracker && bootCfg.KernelPath == "" {
		return nil, nil, nil, fmt.Errorf("--fc requires OCI images (direct kernel boot): Firecracker does not support UEFI/cloudimg boot")
	}
	cmdcore.EnsureFirmwarePath(conf, bootCfg)

	vmID, err := utils.GenerateID()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate VM ID: %w", err)
	}

	nics, _ := cmd.Flags().GetInt("nics")
	netProvider, networkConfigs, err := initNetwork(ctx, conf, vmID, nics, vmCfg, tapQueues(vmCfg.CPU, conf.UseFirecracker))
	if err != nil {
		return nil, nil, nil, err
	}

	info, createErr := hyper.Create(ctx, vmID, vmCfg, storageConfigs, networkConfigs, bootCfg)
	if createErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return nil, nil, nil, fmt.Errorf("create VM: %w", createErr)
	}
	return ctx, info, hyper, nil
}

// tapQueues returns the number of TAP queues per NIC.
// FC only supports single-queue TAPs; CH uses one queue per vCPU.
func tapQueues(cpu int, useFC bool) int {
	if useFC {
		return 1
	}
	return cpu
}

func initNetwork(ctx context.Context, conf *config.Config, vmID string, nics int, vmCfg *types.VMConfig, queues int) (network.Network, []*types.NetworkConfig, error) {
	if nics <= 0 {
		return nil, nil, nil
	}
	netProvider, err := cmdcore.InitNetwork(conf)
	if err != nil {
		return nil, nil, fmt.Errorf("init network: %w", err)
	}
	// Override CPU for TAP queue count — FC uses single-queue, CH uses per-vCPU queues.
	// The network layer derives TAP queues from vmCfg.CPU.
	origCPU := vmCfg.CPU
	vmCfg.CPU = queues
	configs, err := netProvider.Config(ctx, vmID, nics, vmCfg)
	vmCfg.CPU = origCPU
	if err != nil {
		return nil, nil, fmt.Errorf("configure network: %w", err)
	}
	return netProvider, configs, nil
}

func rollbackNetwork(ctx context.Context, netProvider network.Network, vmID string) {
	if netProvider == nil {
		return
	}
	if _, delErr := netProvider.Delete(ctx, []string{vmID}); delErr != nil {
		log.WithFunc("cmd.rollbackNetwork").Warnf(ctx, "rollback network for %s: %v", vmID, delErr)
	}
}

func printPostCloneHints(vm *types.VM, networkConfigs []*types.NetworkConfig) {
	if vm.Config.Windows {
		fmt.Println()
		fmt.Println("Windows clone: NICs hot-swapped with new MAC addresses.")
		fmt.Println("  DHCP networks: no action needed.")
		fmt.Println("  Static IP: configure via SAC serial console (cocoon vm console):")
		fmt.Println("    https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/windows.md")
		fmt.Println()
		return
	}

	isCloudimg := slices.ContainsFunc(vm.StorageConfigs, func(sc *types.StorageConfig) bool {
		return strings.HasSuffix(sc.Path, ".qcow2")
	})

	fmt.Println()
	fmt.Println("Run inside the guest to finish setup:")
	fmt.Println()
	fmt.Println("  # Release memory for balloon")
	fmt.Println("  echo 3 > /proc/sys/vm/drop_caches")

	// FC clone: guest MAC is baked in vmstate (source VM's MAC).
	// Must change guest MAC before networkd config takes effect.
	if vm.Hypervisor == string(config.HypervisorFirecracker) {
		printFCMACHints(networkConfigs)
	}

	if isCloudimg {
		printCloudimgNetworkHints(networkConfigs)
	} else {
		printOCINetworkHints(vm, networkConfigs)
	}
	fmt.Println()
}

// printFCMACHints prints commands to change guest MAC addresses.
// FC clone retains the source VM's MAC in vmstate — must be changed manually.
func printFCMACHints(networkConfigs []*types.NetworkConfig) {
	fmt.Println()
	fmt.Println("  # Fix guest MAC addresses (FC clone retains source VM's MAC)")
	for i, nc := range networkConfigs {
		if nc == nil || nc.Mac == "" {
			continue
		}
		fmt.Printf("  ip link set dev eth%d down\n", i)
		fmt.Printf("  ip link set dev eth%d address %s\n", i, nc.Mac)
		fmt.Printf("  ip link set dev eth%d up\n", i)
	}
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

// validateFCCloneOverrides rejects CPU/memory/NIC overrides for FC clones.
// FC cannot change these after snapshot/load — fail early before creating resources.
func validateFCCloneOverrides(cmd *cobra.Command, cfg *types.SnapshotConfig) error {
	if cpuFlag, _ := cmd.Flags().GetInt("cpu"); cpuFlag > 0 && cpuFlag != cfg.CPU {
		return fmt.Errorf("--cpu %d not supported: Firecracker cannot change CPU after snapshot/load (snapshot has %d)", cpuFlag, cfg.CPU)
	}
	if memStr, _ := cmd.Flags().GetString("memory"); memStr != "" {
		return fmt.Errorf("--memory not supported: Firecracker cannot change memory after snapshot/load")
	}
	if nics, _ := cmd.Flags().GetInt("nics"); nics > 0 && nics != cfg.NICs {
		return fmt.Errorf("--nics %d not supported: Firecracker cannot change NIC count after snapshot/load (snapshot has %d)", nics, cfg.NICs)
	}
	return nil
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
