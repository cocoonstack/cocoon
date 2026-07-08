package cloudhypervisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type cloneResumeOpts struct {
	vmCfg               *types.VMConfig
	directBoot          bool
	hadCidataInSnapshot bool
	storageConfigs      []*types.StorageConfig
	dataDisks           []*types.StorageConfig
	networkConfigs      []*types.NetworkConfig
	snapshotCfg         *chVMConfig
}

func (ch *CloudHypervisor) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, snapshot io.Reader) (*types.VM, error) {
	return ch.CloneFromStream(ctx, vmID, vmCfg, net, snapshotConfig, snapshot, ch.cloneAfterExtract)
}

func (ch *CloudHypervisor) cloneAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, runDir, logDir string, now time.Time, sourceSnapshotID string) (*types.VM, error) {
	networkConfigs := net.NetworkConfigs
	logger := log.WithFunc("cloudhypervisor.Clone")

	chConfigPath := filepath.Join(runDir, configJSONName)
	chCfg, err := parseCHConfig(chConfigPath)
	if err != nil {
		return nil, fmt.Errorf("parse CH config: %w", err)
	}

	meta, err := hypervisor.LoadAndValidateMeta(runDir, ch.conf.RootDir, ch.conf.Config.RunDir)
	if err != nil {
		return nil, fmt.Errorf("load snapshot meta: %w", err)
	}
	if vErr := validateSnapshotIntegrity(runDir, meta.StorageConfigs); vErr != nil {
		return nil, fmt.Errorf("snapshot integrity: %w", vErr)
	}

	storageConfigs := meta.StorageConfigs
	bootCfg := rebuildBootConfig(chCfg)
	directBoot := isDirectBoot(bootCfg)

	cowPath := ch.cowPath(vmID, directBoot)
	if err = updateCOWPath(storageConfigs, cowPath); err != nil {
		return nil, fmt.Errorf("update COW path: %w", err)
	}
	updateDataDiskPaths(storageConfigs, runDir)

	// Snapshot may omit cidata if taken post-restart.
	hadCidataInSnapshot := updateCloneCidataPath(storageConfigs, directBoot, ch.conf.CidataPath(vmID))

	if err = hypervisor.VerifyBaseFiles(storageConfigs, bootCfg); err != nil {
		return nil, fmt.Errorf("verify base files: %w", err)
	}

	stateReplacements := buildStateReplacements(chCfg, storageConfigs)

	storageConfigs, err = ch.ensureCloneCidata(vmID, vmCfg, networkConfigs, storageConfigs, directBoot)
	if err != nil {
		return nil, err
	}
	// The patch list is taken before the new --data-disk configs are appended:
	// they are not in the snapshot's device tree and are hot-added post-restore.
	patchStorageConfigs := restorePatchStorageConfigs(storageConfigs, directBoot, vmCfg.Windows, hadCidataInSnapshot)
	newDataDisks, err := ch.prepareCloneDataDisks(ctx, vmID, vmCfg, storageConfigs)
	if err != nil {
		return nil, err
	}
	storageConfigs = append(storageConfigs, newDataDisks...)
	if vErr := types.ValidateStorageConfigs(storageConfigs); vErr != nil {
		return nil, fmt.Errorf("validate post-cidata storage: %w", vErr)
	}

	consoleSock := hypervisor.ConsoleSockPath(runDir)
	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: patchStorageConfigs,
		consoleSock:    consoleSock,
		vsockSock:      hypervisor.VsockSockPath(runDir),
		directBoot:     directBoot,
		diskQueueSize:  vmCfg.DiskQueueSize,
		noDirectIO:     vmCfg.NoDirectIO,
	}); err != nil {
		return nil, fmt.Errorf("patch CH config: %w", err)
	}

	stateJSONPath := filepath.Join(runDir, stateJSONName)
	if err = patchStateJSON(stateJSONPath, stateReplacements); err != nil {
		return nil, fmt.Errorf("patch state.json: %w", err)
	}

	if directBoot && bootCfg != nil {
		dns, dnsErr := ch.conf.DNSServers()
		if dnsErr != nil {
			return nil, fmt.Errorf("parse DNS servers: %w", dnsErr)
		}
		bootCfg.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, dns)
	}

	sockPath := hypervisor.SocketPath(runDir)
	args := []string{"--api-socket", sockPath}
	ch.saveCmdline(ctx, &hypervisor.VMRecord{RunDir: runDir}, args)

	pid, err := ch.launchProcess(ctx, &hypervisor.VMRecord{
		RunDir: runDir,
		LogDir: logDir,
	}, sockPath, args, net.NetnsPath)
	if err != nil {
		ch.MarkError(ctx, vmID)
		return nil, fmt.Errorf("launch CH: %w", err)
	}

	if err := ch.restoreAndResumeClone(ctx, pid, sockPath, runDir, &cloneResumeOpts{
		vmCfg:               vmCfg,
		directBoot:          directBoot,
		hadCidataInSnapshot: hadCidataInSnapshot,
		storageConfigs:      storageConfigs,
		dataDisks:           newDataDisks,
		networkConfigs:      networkConfigs,
		snapshotCfg:         chCfg,
	}); err != nil {
		return nil, err
	}

	info := &types.VM{
		ID: vmID, Hypervisor: typ, State: types.VMStateRunning,
		Config: *vmCfg, StorageConfigs: storageConfigs,
		NetSetup:  net,
		CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	if err := ch.FinalizeClone(ctx, vmID, info, bootCfg, nil, sourceSnapshotID); err != nil {
		ch.AbortLaunch(ctx, pid, sockPath, runDir, runtimeFiles)
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}

	logger.Infof(ctx, "VM %s cloned from snapshot", vmID)
	return info, nil
}

func (ch *CloudHypervisor) restoreAndResumeClone(
	ctx context.Context,
	pid int,
	sockPath, runDir string,
	opts *cloneResumeOpts,
) (err error) {
	defer func() {
		if err != nil {
			ch.AbortLaunch(ctx, pid, sockPath, runDir, runtimeFiles)
		}
	}()

	hc := utils.NewSocketHTTPClient(sockPath)

	if err = restoreVM(ctx, hc, runDir, opts.vmCfg.OnDemand); err != nil {
		return fmt.Errorf("vm.restore: %w", err)
	}

	if err = hotSwapNets(ctx, hc, opts.snapshotCfg.Nets, opts.networkConfigs); err != nil {
		return fmt.Errorf("hot-swap NICs: %w", err)
	}

	if !opts.directBoot && !opts.vmCfg.Windows && !opts.hadCidataInSnapshot {
		if len(opts.storageConfigs) == 0 {
			return fmt.Errorf("vm.add-disk (cidata): missing storage config")
		}
		cidataDisk := storageConfigToDisk(opts.storageConfigs[len(opts.storageConfigs)-1], opts.vmCfg.CPU, opts.vmCfg.DiskQueueSize, opts.vmCfg.NoDirectIO)
		if err = addDiskVM(ctx, hc, cidataDisk); err != nil {
			return fmt.Errorf("vm.add-disk (cidata): %w", err)
		}
	}
	for _, sc := range opts.dataDisks {
		if err = addDiskVM(ctx, hc, storageConfigToDisk(sc, opts.vmCfg.CPU, opts.vmCfg.DiskQueueSize, opts.vmCfg.NoDirectIO)); err != nil {
			return fmt.Errorf("vm.add-disk (data %s): %w", sc.Serial, err)
		}
	}
	if err = resumeVM(ctx, hc); err != nil {
		return fmt.Errorf("vm.resume: %w", err)
	}
	return nil
}

// prepareCloneDataDisks creates the --data-disk files requested for a clone.
// The snapshot's device tree cannot grow at restore, so these are hot-added
// after resume; a name colliding with an inherited disk's serial would
// overwrite its backing file in the clone run dir.
func (ch *CloudHypervisor) prepareCloneDataDisks(ctx context.Context, vmID string, vmCfg *types.VMConfig, existing []*types.StorageConfig) ([]*types.StorageConfig, error) {
	if len(vmCfg.DataDisks) == 0 {
		return nil, nil
	}
	for _, spec := range vmCfg.DataDisks {
		for _, sc := range existing {
			if sc.Serial == spec.Name {
				return nil, fmt.Errorf("--data-disk name %q collides with a disk inherited from the snapshot", spec.Name)
			}
		}
	}
	return hypervisor.PrepareDataDisks(ctx, ch.conf.VMRunDir(vmID), vmCfg.DataDisks)
}

func (ch *CloudHypervisor) ensureCloneCidata(vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, storageConfigs []*types.StorageConfig, directBoot bool) ([]*types.StorageConfig, error) {
	if directBoot || vmCfg.Windows {
		return storageConfigs, nil
	}
	if err := ch.generateCidata(vmID, vmCfg, networkConfigs, storageConfigs); err != nil {
		return nil, fmt.Errorf("generate cidata: %w", err)
	}
	cidataPath := ch.conf.CidataPath(vmID)
	// Keep cidata in record for later cold boots even if snapshot omitted it.
	if !slices.ContainsFunc(storageConfigs, hasCidataRole) {
		storageConfigs = append(storageConfigs, &types.StorageConfig{
			Path: cidataPath,
			RO:   true,
			Role: types.StorageRoleCidata,
		})
	}
	return storageConfigs, nil
}

func parseCHConfig(path string) (*chVMConfig, error) {
	var cfg chVMConfig
	if err := utils.ReadJSONFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func rebuildBootConfig(cfg *chVMConfig) *types.BootConfig {
	if cfg.Payload == nil {
		return nil
	}
	p := cfg.Payload
	if p.Kernel == "" && p.Firmware == "" {
		return nil
	}
	return &types.BootConfig{
		KernelPath:   p.Kernel,
		InitrdPath:   p.Initramfs,
		Cmdline:      p.Cmdline,
		FirmwarePath: p.Firmware,
	}
}

func updateCloneCidataPath(storageConfigs []*types.StorageConfig, directBoot bool, cidataPath string) bool {
	if directBoot {
		return false
	}
	hadCidataInSnapshot := false
	for _, sc := range storageConfigs {
		if sc.Role == types.StorageRoleCidata {
			sc.Path = cidataPath
			hadCidataInSnapshot = true
		}
	}
	return hadCidataInSnapshot
}

func hasCidataRole(sc *types.StorageConfig) bool {
	return sc.Role == types.StorageRoleCidata
}

// restorePatchStorageConfigs drops the appended cidata when the snapshot lacked one (cidata gets hot-plugged).
func restorePatchStorageConfigs(storageConfigs []*types.StorageConfig, directBoot, windows, hadCidataInSnapshot bool) []*types.StorageConfig {
	if directBoot || windows || hadCidataInSnapshot {
		return storageConfigs
	}
	out := make([]*types.StorageConfig, 0, len(storageConfigs))
	for _, sc := range storageConfigs {
		if sc.Role != types.StorageRoleCidata {
			out = append(out, sc)
		}
	}
	return out
}

// updateDataDiskPaths rewrites Role==Data paths to clone runDir; sidecar carries source paths.
func updateDataDiskPaths(configs []*types.StorageConfig, newRunDir string) {
	for _, sc := range configs {
		if sc.Role == types.StorageRoleData {
			sc.Path = filepath.Join(newRunDir, hypervisor.DataDiskBaseName(sc.Serial))
		}
	}
}

func updateCOWPath(configs []*types.StorageConfig, newCOWPath string) error {
	found := false
	for _, sc := range configs {
		if sc.Role == types.StorageRoleCOW {
			sc.Path = newCOWPath
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no COW disk found in storage configs")
	}
	return nil
}

func buildCmdline(storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, vmName string, dnsServers []string) string {
	return hypervisor.BuildBaseCmdline(
		"console=hvc0 loglevel=3",
		strings.Join(ReverseLayerSerials(storageConfigs), ","),
		hypervisor.CowSerial,
		networkConfigs, vmName, dnsServers,
	)
}

// buildStateReplacements maps source→clone disk paths for state.json; min-length slice keeps appended cidata aligned.
func buildStateReplacements(chCfg *chVMConfig, storageConfigs []*types.StorageConfig) map[string]string {
	n := min(len(chCfg.Disks), len(storageConfigs))
	m := make(map[string]string, n)
	for i := range n {
		if storageConfigs[i].Path != chCfg.Disks[i].Path {
			m[chCfg.Disks[i].Path] = storageConfigs[i].Path
		}
	}
	return m
}

// hotSwapNets removes NICs with stale MAC (from snapshot binary state) and adds fresh ones. Must run between vm.restore and vm.resume (VM paused).
func hotSwapNets(ctx context.Context, hc *http.Client, oldNets []chNet, networkConfigs []*types.NetworkConfig) error {
	logger := log.WithFunc("cloudhypervisor.hotSwapNets")
	for _, oldNet := range oldNets {
		if oldNet.ID == "" {
			continue
		}
		if err := removeDeviceVM(ctx, hc, oldNet.ID); err != nil {
			return fmt.Errorf("remove net device %s: %w", oldNet.ID, err)
		}
		logger.Infof(ctx, "removed snapshot NIC %s (old MAC %s)", oldNet.ID, oldNet.MAC)
	}
	for i, nc := range networkConfigs {
		if _, err := addCocoonNIC(ctx, hc, nc); err != nil {
			return fmt.Errorf("add net device %d/%d (MAC %s, TAP %s): %w",
				i+1, len(networkConfigs), nc.MAC, nc.TAP, err)
		}
		logger.Infof(ctx, "added NIC with MAC %s on TAP %s", nc.MAC, nc.TAP)
	}
	return nil
}
