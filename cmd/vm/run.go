package vm

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type nicHint struct {
	mac, ip, gw string
	prefix      int
}

func (h Handler) Create(cmd *cobra.Command, args []string) error {
	ctx, vm, _, err := h.createVM(cmd, args[0])
	if err != nil {
		return err
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, vm); done {
		return jsonErr
	}
	logger := log.WithFunc("cmd.vm.create")
	logger.Infof(ctx, "VM created: %s (name: %s, state: %s)", vm.ID, vm.Config.Name, vm.State)
	logger.Infof(ctx, "start with: cocoon vm start %s", vm.ID)
	return nil
}

func (h Handler) Run(cmd *cobra.Command, args []string) error {
	ctx, vm, hyper, err := h.createVM(cmd, args[0])
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.vm.run")
	wantJSON := cliutil.WantJSON(cmd)
	if !wantJSON {
		logger.Infof(ctx, "VM created: %s (name: %s)", vm.ID, vm.Config.Name)
	}

	started, err := hyper.Start(ctx, []string{vm.ID})
	if err != nil {
		return fmt.Errorf("start VM %s: %w", vm.ID, err)
	}
	if wantJSON {
		info, inspectErr := hyper.Inspect(ctx, vm.ID)
		switch {
		case inspectErr != nil:
			logger.Warnf(ctx, "inspect after start failed: %v (json payload may be stale)", inspectErr)
		case info != nil:
			vm = info
		}
		return cliutil.OutputJSON(vm)
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
	logger := log.WithFunc("cmd.vm.clone")

	fromDir, snapRef, err := snapshotSource(cmd, args, 0)
	if err != nil {
		return err
	}
	if fromDir != "" {
		return h.cloneFromDir(ctx, cmd, conf, fromDir, logger)
	}

	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}

	snapInfo, err := snapBackend.Inspect(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("inspect snapshot %s: %w", snapRef, err)
	}
	if snapInfo.Hypervisor != "" {
		conf.UseFirecracker = snapInfo.Hypervisor == string(config.HypervisorFirecracker)
	}

	hyper, err := cmdcore.InitHypervisor(ctx, conf)
	if err != nil {
		return err
	}

	if da, ok := snapBackend.(snapshot.Direct); ok {
		if dcr, ok := hyper.(hypervisor.Direct); ok {
			return h.cloneDirect(ctx, cmd, conf, hyper, dcr, da, snapRef, logger)
		}
	}

	cfg, stream, err := snapBackend.Restore(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}
	defer stream.Close() //nolint:errcheck
	defer cmdcore.CloseOnCancel(ctx, stream)()

	vmCfg, vmID, unlock, netProvider, netSetup, err := h.prepareClone(ctx, cmd, conf, hyper, cfg)
	if err != nil {
		return err
	}
	defer unlock()

	logger.Infof(ctx, "cloning VM from snapshot %s ...", snapRef)

	vm, cloneErr := hyper.Clone(ctx, vmID, vmCfg, netSetup, &cfg, stream)
	if cloneErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return fmt.Errorf("clone VM: %w", cloneErr)
	}
	h.reseedAfterResume(ctx, hyper, vm, true)

	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, vm); done {
		return jsonErr
	}
	logger.Infof(ctx, "VM cloned: %s (name: %s)", vm.ID, vm.Config.Name)
	printPostCloneHints(vm)
	return nil
}

func (h Handler) Restore(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.vm.restore")

	vmRef := args[0]
	fromDir, snapRef, err := snapshotSource(cmd, args, 1)
	if err != nil {
		return err
	}
	if fromDir != "" {
		return h.restoreFromDir(ctx, cmd, conf, vmRef, fromDir, logger)
	}

	hyper, err := cmdcore.FindHypervisor(ctx, conf, vmRef)
	if err != nil {
		return fmt.Errorf("find VM %s: %w", vmRef, err)
	}
	h.recoverNetworkForStopped(ctx, conf, hyper, vmRef)
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
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

	vmCfg, err := cmdcore.RestoreVMConfigFromFlags(cmd, vm, snapInfo.SnapshotConfig)
	if err != nil {
		return err
	}

	done, directErr := h.restoreDirect(ctx, cmd, snapRef, vmRef, vmCfg, snapBackend, hyper, logger)
	if done {
		return directErr
	}

	_, stream, err := snapBackend.Restore(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer stream.Close() //nolint:errcheck
	defer cmdcore.CloseOnCancel(ctx, stream)()

	logger.Infof(ctx, "restoring VM %s from snapshot %s ...", vmRef, snapRef)

	result, err := hyper.Restore(ctx, vmRef, vmCfg, stream, snapInfo.ID)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	h.reseedAfterResume(ctx, hyper, result, false)

	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, result); done {
		return jsonErr
	}
	logger.Infof(ctx, "VM %s restored (state: %s)", result.ID, result.State)
	return nil
}

// restoreFromDir runs DirectRestore over an envelope dir; a foreign snapshot ID requires --force so cross-lineage overwrite is opt-in.
func (h Handler) restoreFromDir(ctx context.Context, cmd *cobra.Command, conf *config.Config, vmRef, dir string, logger *log.Fields) error {
	cfg, err := snapshot.ReadSnapshotEnvelope(dir)
	if err != nil {
		return fmt.Errorf("load envelope: %w", err)
	}
	hyper, err := cmdcore.FindHypervisor(ctx, conf, vmRef)
	if err != nil {
		return fmt.Errorf("find VM %s: %w", vmRef, err)
	}
	dcr, ok := hyper.(hypervisor.Direct)
	if !ok {
		return fmt.Errorf("backend %s does not support direct restore", hyper.Type())
	}
	vm, err := hyper.Inspect(ctx, vmRef)
	if err != nil {
		return fmt.Errorf("inspect VM: %w", err)
	}
	if vm.State != types.VMStateRunning {
		h.recoverNetwork(ctx, conf, hyper, []string{vm.ID})
	}
	if _, owned := vm.SnapshotIDs[cfg.ID]; !owned {
		force, _ := cmd.Flags().GetBool("force")
		if !force {
			return fmt.Errorf("snapshot envelope id %s does not belong to VM %s; pass --force to override", cfg.ID, vmRef)
		}
		logger.Warnf(ctx, "snapshot envelope id %s does not belong to VM %s; --force in effect", cfg.ID, vmRef)
	}
	vmCfg, err := cmdcore.RestoreVMConfigFromFlags(cmd, vm, cfg)
	if err != nil {
		return err
	}
	return h.runDirectRestore(ctx, cmd, hyper, dcr, vmRef, vmCfg, dir, cfg.ID,
		fmt.Sprintf("dir %s", dir), logger)
}

func (h Handler) cloneDirect(ctx context.Context, cmd *cobra.Command, conf *config.Config, hyper hypervisor.Hypervisor, dcr hypervisor.Direct, da snapshot.Direct, snapRef string, logger *log.Fields) error {
	dataDir, cfg, release, err := da.DataDir(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}
	defer release()
	return h.cloneFromSrcDir(ctx, cmd, conf, hyper, dcr, cfg, dataDir,
		fmt.Sprintf("snapshot %s (direct)", snapRef), logger)
}

// cloneFromDir runs DirectClone over an envelope-bearing dir. The dir stays read-only across the call so concurrent clones of a golden image are safe.
func (h Handler) cloneFromDir(ctx context.Context, cmd *cobra.Command, conf *config.Config, dir string, logger *log.Fields) error {
	cfg, err := snapshot.ReadSnapshotEnvelope(dir)
	if err != nil {
		return fmt.Errorf("load envelope: %w", err)
	}
	if conf == nil {
		return fmt.Errorf("nil config")
	}
	// Local copy keeps backend flip from leaking to the caller's shared *config.Config.
	localConf := *conf
	if cfg.Hypervisor != "" {
		localConf.UseFirecracker = cfg.Hypervisor == string(config.HypervisorFirecracker)
	}
	hyper, err := cmdcore.InitHypervisor(ctx, &localConf)
	if err != nil {
		return err
	}
	dcr, ok := hyper.(hypervisor.Direct)
	if !ok {
		return fmt.Errorf("backend %s does not support direct clone", hyper.Type())
	}
	return h.cloneFromSrcDir(ctx, cmd, &localConf, hyper, dcr, cfg, dir,
		fmt.Sprintf("dir %s", dir), logger)
}

func (h Handler) cloneFromSrcDir(ctx context.Context, cmd *cobra.Command, conf *config.Config, hyper hypervisor.Hypervisor, dcr hypervisor.Direct, cfg types.SnapshotConfig, srcDir, sourceLabel string, logger *log.Fields) error {
	vmCfg, vmID, unlock, netProvider, netSetup, err := h.prepareClone(ctx, cmd, conf, hyper, cfg)
	if err != nil {
		return err
	}
	defer unlock()

	wantJSON := cliutil.WantJSON(cmd)
	if !wantJSON {
		logger.Infof(ctx, "cloning VM from %s ...", sourceLabel)
	}

	vm, cloneErr := dcr.DirectClone(ctx, vmID, vmCfg, netSetup, &cfg, srcDir)
	if cloneErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return fmt.Errorf("clone VM: %w", cloneErr)
	}
	h.reseedAfterResume(ctx, hyper, vm, true)

	if wantJSON {
		return cliutil.OutputJSON(vm)
	}
	logger.Infof(ctx, "VM cloned: %s (name: %s)", vm.ID, vm.Config.Name)
	printPostCloneHints(vm)
	return nil
}

func (h Handler) prepareClone(ctx context.Context, cmd *cobra.Command, conf *config.Config, hyper hypervisor.Hypervisor, cfg types.SnapshotConfig) (*types.VMConfig, string, func(), network.Network, types.NetSetup, error) {
	vmCfg, err := cmdcore.CloneVMConfigFromFlags(cmd, cfg)
	if err != nil {
		return nil, "", nil, nil, types.NetSetup{}, err
	}
	vmID := utils.GenerateID()
	if vmCfg.Name == "" {
		vmCfg.Name = "cocoon-clone-" + network.VMIDPrefix(vmID)
	}
	if err = vmCfg.Validate(); err != nil {
		return nil, "", nil, nil, types.NetSetup{}, err
	}
	rollbackReserve, unlock, err := prereserveVM(ctx, hyper, vmID, vmCfg)
	if err != nil {
		return nil, "", nil, nil, types.NetSetup{}, err
	}
	fail := func(err error) (*types.VMConfig, string, func(), network.Network, types.NetSetup, error) {
		rollbackReserve()
		unlock()
		return nil, "", nil, nil, types.NetSetup{}, err
	}

	if pull, _ := cmd.Flags().GetBool("pull"); pull && vmCfg.Image != "" && vmCfg.ImageType != "" {
		backends, initErr := cmdcore.InitImageBackends(ctx, conf)
		if initErr != nil {
			return fail(fmt.Errorf("init image backends: %w", initErr))
		}
		cmdcore.EnsureImage(ctx, backends, vmCfg)
	}

	bridgeDev, _ := cmd.Flags().GetString("bridge")
	nics := cfg.NICs
	if cmd.Flags().Changed("nics") {
		if conf.UseFirecracker {
			return fail(fmt.Errorf("--nics override on clone is Cloud Hypervisor only (FC network_overrides retargets existing NICs, not resize)"))
		}
		nics, _ = cmd.Flags().GetInt("nics")
	}
	// Pre-extract fast-fail; fc's clone-extract guard stays as the library backstop.
	if len(vmCfg.DataDisks) > 0 && conf.UseFirecracker {
		return fail(fmt.Errorf("--data-disk on clone is Cloud Hypervisor only (Firecracker has no disk hotplug): %w", disk.ErrUnsupportedBackend))
	}
	netProvider, netSetup, err := initNetwork(ctx, conf, vmID, nics, vmCfg, tapQueues(vmCfg.CPU, conf.UseFirecracker), bridgeDev)
	if err != nil {
		return fail(err)
	}

	return vmCfg, vmID, unlock, netProvider, netSetup, nil
}

func (h Handler) restoreDirect(ctx context.Context, cmd *cobra.Command, snapRef, vmRef string, vmCfg *types.VMConfig, snapBackend snapshot.Snapshot, hyper hypervisor.Hypervisor, logger *log.Fields) (bool, error) {
	da, ok := snapBackend.(snapshot.Direct)
	if !ok {
		return false, nil
	}
	dcr, ok := hyper.(hypervisor.Direct)
	if !ok {
		return false, nil
	}
	dataDir, snapCfg, release, err := da.DataDir(ctx, snapRef)
	if err != nil {
		return true, fmt.Errorf("open snapshot: %w", err)
	}
	defer release()
	return true, h.runDirectRestore(ctx, cmd, hyper, dcr, vmRef, vmCfg, dataDir, snapCfg.ID,
		fmt.Sprintf("snapshot %s", snapRef), logger)
}

// runDirectRestore is the shared tail for the snapshot-DB and --from-dir restore paths: log, DirectRestore, output.
func (h Handler) runDirectRestore(ctx context.Context, cmd *cobra.Command, hyper hypervisor.Hypervisor, dcr hypervisor.Direct, vmRef string, vmCfg *types.VMConfig, srcDir, sourceSnapshotID, sourceLabel string, logger *log.Fields) error {
	wantJSON := cliutil.WantJSON(cmd)
	if !wantJSON {
		logger.Infof(ctx, "restoring VM %s from %s (direct) ...", vmRef, sourceLabel)
	}
	result, err := dcr.DirectRestore(ctx, vmRef, vmCfg, srcDir, sourceSnapshotID)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	h.reseedAfterResume(ctx, hyper, result, false)
	if wantJSON {
		return cliutil.OutputJSON(result)
	}
	logger.Infof(ctx, "VM %s restored (state: %s)", result.ID, result.State)
	return nil
}

func (h Handler) createVM(cmd *cobra.Command, image string) (context.Context, *types.VM, hypervisor.Hypervisor, error) {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	if fc, _ := cmd.Flags().GetBool("fc"); fc {
		conf.UseFirecracker = true
	}

	vmCfg, err := cmdcore.VMConfigFromFlags(cmd, image)
	if err != nil {
		return nil, nil, nil, err
	}

	if conf.UseFirecracker && vmCfg.Windows {
		return nil, nil, nil, fmt.Errorf("--fc and --windows are mutually exclusive: Firecracker does not support Windows guests")
	}
	if conf.UseFirecracker && vmCfg.SharedMemory {
		return nil, nil, nil, fmt.Errorf("--fc and --shared-memory are mutually exclusive: Firecracker does not support vhost-user-fs hot-plug")
	}
	bridgeDev, _ := cmd.Flags().GetString("bridge")
	if bridgeDev != "" && vmCfg.Network != "" {
		return nil, nil, nil, fmt.Errorf("--bridge and --network are mutually exclusive")
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

	vmID := utils.GenerateID()
	rollbackReserve, unlock, err := prereserveVM(ctx, hyper, vmID, vmCfg)
	if err != nil {
		return nil, nil, nil, err
	}
	defer unlock()

	nics, _ := cmd.Flags().GetInt("nics")
	netProvider, netSetup, err := initNetwork(ctx, conf, vmID, nics, vmCfg, tapQueues(vmCfg.CPU, conf.UseFirecracker), bridgeDev)
	if err != nil {
		rollbackReserve()
		return nil, nil, nil, err
	}

	// A Create failure rolls back the adopted placeholder itself.
	info, createErr := hyper.Create(ctx, vmID, vmCfg, storageConfigs, netSetup, bootCfg)
	if createErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return nil, nil, nil, fmt.Errorf("create VM: %w", createErr)
	}
	return ctx, info, hyper, nil
}

// prereserveVM locks the VM's ops and claims its ID before network provisioning, so GC never sees ownerless TAP/netns and rm/start cannot interleave until the backend finalizes. rollback covers failures before the backend adopts the placeholder; unlock is deferred past Create/Clone by the caller.
func prereserveVM(ctx context.Context, hyper hypervisor.Hypervisor, vmID string, vmCfg *types.VMConfig) (rollback, unlock func(), err error) {
	r, ok := hyper.(hypervisor.Reserver)
	if !ok {
		return func() {}, func() {}, nil
	}
	unlock, err = r.LockVMOps(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}
	if err := r.PrereserveVM(ctx, vmID, vmCfg); err != nil {
		unlock()
		return nil, nil, fmt.Errorf("reserve VM record: %w", err)
	}
	return func() { r.RollbackCreate(ctx, vmID, vmCfg.Name) }, unlock, nil
}

// snapshotSource picks the clone/restore source: --from-dir or args[baseArgs]. Exactly one of (fromDir, snapRef) is non-empty.
func snapshotSource(cmd *cobra.Command, args []string, baseArgs int) (string, string, error) {
	fromDir, _ := cmd.Flags().GetString("from-dir")
	if fromDir != "" {
		if len(args) > baseArgs {
			return "", "", fmt.Errorf("--from-dir and positional SNAPSHOT are mutually exclusive")
		}
		return fromDir, "", nil
	}
	if len(args) <= baseArgs {
		return "", "", fmt.Errorf("snapshot is required (or use --from-dir)")
	}
	return "", args[baseArgs], nil
}

// tapQueues sizes the TAP queue count: FC opens the TAP single-queue, CH per-vCPU.
func tapQueues(cpu int, useFC bool) int {
	if useFC {
		return network.NetNumQueues(1)
	}
	return network.NetNumQueues(cpu)
}

func initNetwork(ctx context.Context, conf *config.Config, vmID string, nics int, vmCfg *types.VMConfig, queues int, bridgeDev string) (network.Network, types.NetSetup, error) {
	var netProvider network.Network
	var err error
	if bridgeDev != "" {
		netProvider, err = cmdcore.InitBridgeNetwork(conf, bridgeDev)
	} else {
		netProvider, err = cmdcore.InitNetwork(conf)
	}
	if err != nil {
		return nil, types.NetSetup{}, fmt.Errorf("init network: %w", err)
	}
	nsPath, err := netProvider.Prepare(ctx, vmID, vmCfg)
	if err != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return nil, types.NetSetup{}, fmt.Errorf("prepare network: %w", err)
	}
	backend := netProvider.Type()
	// CNI no-conflist + 0 NICs runs in host netns; empty backend so resize won't mispick CNI.
	if nics <= 0 && backend == types.BackendCNI && nsPath == "" {
		return netProvider, types.NetSetup{}, nil
	}
	setup := types.NetSetup{NetBackend: backend, NetnsPath: nsPath, NetBridgeDev: bridgeDev}
	if nics <= 0 {
		return netProvider, setup, nil
	}
	specs := network.AddRange(0, nics)
	for i := range specs {
		specs[i].Queues = queues
	}
	configs, err := netProvider.Add(ctx, vmID, vmCfg, specs...)
	if err != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return nil, types.NetSetup{}, fmt.Errorf("configure network: %w", err)
	}
	setup.NetworkConfigs = configs
	return netProvider, setup, nil
}

func rollbackNetwork(ctx context.Context, netProvider network.Network, vmID string) {
	if netProvider == nil {
		return
	}
	if _, delErr := netProvider.Delete(ctx, []string{vmID}); delErr != nil {
		log.WithFunc("cmd.vm.rollbackNetwork").Warnf(ctx, "rollback network for %s: %v", vmID, delErr)
	}
}

func printPostCloneHints(vm *types.VM) {
	if vm.Config.Windows {
		fmt.Println()
		fmt.Println("Windows clone: NICs hot-swapped with new MAC addresses.")
		fmt.Println("Run inside the guest if no IPv4 (DHCP):")
		fmt.Println()
		fmt.Println(`  powershell -nop -c '$x=Get-PnpDevice -Class Net -PresentOnly;$x|Disable-PnpDevice -Confirm:$false;$x|Enable-PnpDevice -Confirm:$false'`)
		fmt.Println()
		fmt.Println("Static IP: run inside the guest after the rebind above, e.g.:")
		fmt.Println()
		fmt.Println(`  netsh interface ipv4 set address name="Ethernet" static <IP> <MASK> <GATEWAY>`)
		fmt.Println()
		return
	}

	isCloudimg := vm.Config.ImageType == types.ImageTypeCloudImg

	fmt.Println()
	fmt.Println("Run inside the guest to finish setup:")
	fmt.Println()
	fmt.Println("  # Release memory for balloon")
	fmt.Println("  echo 3 > /proc/sys/vm/drop_caches")

	// FC clone: guest MAC is baked in vmstate; change it before networkd config.
	if vm.Hypervisor == string(config.HypervisorFirecracker) {
		printFCMACHints(vm.NetworkConfigs)
	}

	fmt.Println()
	fmt.Println("  # Clean old network configs from snapshot and write new ones (MAC-based)")
	fmt.Println("  rm -f /etc/systemd/network/10-*.network")

	if isCloudimg {
		printCloudimgNetworkHints()
	} else {
		printOCINetworkHints(vm)
	}
	fmt.Println()
}

func printFCMACHints(networkConfigs []*types.NetworkConfig) {
	fmt.Println()
	fmt.Println("  # Fix guest MAC addresses (FC clone retains source VM's MAC)")
	for i, nc := range networkConfigs {
		if nc == nil || nc.MAC == "" {
			continue
		}
		fmt.Printf("  ip link set dev eth%d down\n", i)
		fmt.Printf("  ip link set dev eth%d address %s\n", i, nc.MAC)
		fmt.Printf("  ip link set dev eth%d up\n", i)
	}
}

func printCloudimgNetworkHints() {
	fmt.Println("  cloud-init clean --logs --seed --configs network && cloud-init init --local && cloud-init init")
	fmt.Println("  cloud-init modules --mode=config && systemctl restart systemd-networkd")
}

func printOCINetworkHints(vm *types.VM) {
	fmt.Println()
	fmt.Printf("  # Set hostname\n")
	fmt.Printf("  hostnamectl set-hostname %s\n", vm.Config.Name)

	var staticNICs []nicHint
	var dhcpMACs []string
	for _, nc := range vm.NetworkConfigs {
		if nc == nil || nc.MAC == "" {
			continue
		}
		if nc.Network != nil && nc.Network.IP != "" {
			staticNICs = append(staticNICs, nicHint{
				mac:    nc.MAC,
				ip:     nc.Network.IP,
				prefix: nc.Network.Prefix,
				gw:     nc.Network.Gateway,
			})
		} else {
			dhcpMACs = append(dhcpMACs, nc.MAC)
		}
	}

	if len(staticNICs) == 0 && len(dhcpMACs) == 0 {
		return
	}

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
