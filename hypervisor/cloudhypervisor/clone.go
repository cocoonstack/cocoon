package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	directBoot          bool
	windows             bool
	hadCidataInSnapshot bool
	storageConfigs      []*types.StorageConfig
	networkConfigs      []*types.NetworkConfig
	snapshotCfg         *chVMConfig
	cpu                 int
	diskQueueSize       int
	noDirectIO          bool
	onDemand            bool
}

// Clone creates a new VM from a snapshot tar stream.
func (ch *CloudHypervisor) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, snapshotConfig *types.SnapshotConfig, snapshot io.Reader) (*types.VM, error) {
	return ch.CloneFromStream(ctx, vmID, vmCfg, networkConfigs, snapshotConfig, snapshot, ch.cloneAfterExtract)
}

// cloneAfterExtract resumes from snapshot data already placed in runDir.
func (ch *CloudHypervisor) cloneAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, runDir, logDir string, now time.Time) (*types.VM, error) {
	logger := log.WithFunc("cloudhypervisor.Clone")

	chConfigPath := filepath.Join(runDir, "config.json")
	chCfg, chConfigRaw, err := parseCHConfig(chConfigPath)
	if err != nil {
		return nil, fmt.Errorf("parse CH config: %w", err)
	}

	meta, err := loadSnapshotMeta(runDir)
	if err != nil {
		return nil, fmt.Errorf("load snapshot meta: %w", err)
	}
	if err := validateSnapshotIntegrity(runDir, meta.StorageConfigs); err != nil {
		return nil, fmt.Errorf("snapshot integrity: %w", err)
	}

	storageConfigs := meta.StorageConfigs
	bootCfg := rebuildBootConfig(chCfg)
	directBoot := isDirectBoot(bootCfg)

	cowPath := ch.cowPath(vmID, directBoot)
	if err = updateCOWPath(storageConfigs, cowPath); err != nil {
		return nil, fmt.Errorf("update COW path: %w", err)
	}
	updateDataDiskPaths(storageConfigs, runDir)

	// Snapshot may omit cidata if taken after restart.
	hadCidataInSnapshot := updateCloneCidataPath(storageConfigs, directBoot, ch.conf.CidataPath(vmID))

	if err = hypervisor.VerifyBaseFiles(storageConfigs, bootCfg); err != nil {
		return nil, fmt.Errorf("verify base files: %w", err)
	}
	if vmCfg.Storage > 0 {
		if err = qemuExpandImage(ctx, cowPath, vmCfg.Storage, directBoot); err != nil {
			return nil, fmt.Errorf("resize COW: %w", err)
		}
	}

	stateReplacements := buildStateReplacements(chCfg, storageConfigs)

	// Regenerate cidata for the clone's identity and network config.
	storageConfigs, err = ch.ensureCloneCidata(vmID, vmCfg, networkConfigs, storageConfigs, directBoot)
	if err != nil {
		return nil, err
	}
	if err := types.ValidateStorageConfigs(storageConfigs); err != nil {
		return nil, fmt.Errorf("validate post-cidata storage: %w", err)
	}

	// If the snapshot lacked cidata, patch only snapshot disks and hotplug cidata later.
	patchStorageConfigs := restorePatchStorageConfigs(storageConfigs, directBoot, vmCfg.Windows, hadCidataInSnapshot)

	consoleSock := hypervisor.ConsoleSockPath(runDir)
	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: patchStorageConfigs,
		consoleSock:    consoleSock,
		directBoot:     directBoot,
		windows:        vmCfg.Windows,
		cpu:            vmCfg.CPU,
		memory:         vmCfg.Memory,
		diskQueueSize:  vmCfg.DiskQueueSize,
		noDirectIO:     vmCfg.NoDirectIO,
	}, chCfg, chConfigRaw); err != nil {
		return nil, fmt.Errorf("patch CH config: %w", err)
	}

	// Keep state.json disk paths readable after cloning.
	stateJSONPath := filepath.Join(runDir, "state.json")
	if err = patchStateJSON(stateJSONPath, stateReplacements); err != nil {
		return nil, fmt.Errorf("patch state.json: %w", err)
	}

	// Refresh direct-boot cmdline for later restarts.
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

	withNetwork := len(networkConfigs) > 0
	pid, err := ch.launchProcess(ctx, &hypervisor.VMRecord{
		VM:     types.VM{NetworkConfigs: networkConfigs},
		RunDir: runDir,
		LogDir: logDir,
	}, sockPath, args, withNetwork)
	if err != nil {
		ch.MarkError(ctx, vmID)
		return nil, fmt.Errorf("launch CH: %w", err)
	}

	if err := ch.restoreAndResumeClone(ctx, pid, sockPath, runDir, &cloneResumeOpts{
		directBoot:          directBoot,
		windows:             vmCfg.Windows,
		hadCidataInSnapshot: hadCidataInSnapshot,
		storageConfigs:      storageConfigs,
		networkConfigs:      networkConfigs,
		snapshotCfg:         chCfg,
		cpu:                 vmCfg.CPU,
		diskQueueSize:       vmCfg.DiskQueueSize,
		noDirectIO:          vmCfg.NoDirectIO,
		onDemand:            vmCfg.OnDemand,
	}); err != nil {
		return nil, err
	}

	info := &types.VM{
		ID: vmID, Hypervisor: typ, State: types.VMStateRunning,
		Config: *vmCfg, StorageConfigs: storageConfigs, NetworkConfigs: networkConfigs,
		CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	if err := ch.FinalizeClone(ctx, vmID, info, bootCfg, nil); err != nil {
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

	if err = restoreVM(ctx, hc, runDir, opts.onDemand); err != nil {
		return fmt.Errorf("vm.restore: %w", err)
	}

	if err = hotSwapNets(ctx, hc, opts.snapshotCfg.Nets, opts.networkConfigs); err != nil {
		return fmt.Errorf("hot-swap NICs: %w", err)
	}

	if !opts.directBoot && !opts.windows && !opts.hadCidataInSnapshot {
		if len(opts.storageConfigs) == 0 {
			return fmt.Errorf("vm.add-disk (cidata): missing storage config")
		}
		cidataDisk := storageConfigToDisk(opts.storageConfigs[len(opts.storageConfigs)-1], opts.cpu, opts.diskQueueSize, opts.noDirectIO)
		if err = addDiskVM(ctx, hc, cidataDisk); err != nil {
			return fmt.Errorf("vm.add-disk (cidata): %w", err)
		}
	}
	if err = resumeVM(ctx, hc); err != nil {
		return fmt.Errorf("vm.resume: %w", err)
	}
	return nil
}

func (ch *CloudHypervisor) ensureCloneCidata(vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, storageConfigs []*types.StorageConfig, directBoot bool) ([]*types.StorageConfig, error) {
	if directBoot || vmCfg.Windows {
		return storageConfigs, nil
	}
	if err := ch.generateCidata(vmID, vmCfg, networkConfigs); err != nil {
		return nil, fmt.Errorf("generate cidata: %w", err)
	}
	cidataPath := ch.conf.CidataPath(vmID)
	// Keep cidata in the record for later cold boots even if the snapshot omitted it.
	if !slices.ContainsFunc(storageConfigs, hasCidataRole) {
		storageConfigs = append(storageConfigs, &types.StorageConfig{
			Path: cidataPath,
			RO:   true,
			Role: types.StorageRoleCidata,
		})
	}
	return storageConfigs, nil
}

func parseCHConfig(path string) (*chVMConfig, []byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg chVMConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &cfg, data, nil
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

// hasCidataRole is a slices.ContainsFunc predicate.
func hasCidataRole(sc *types.StorageConfig) bool {
	return sc.Role == types.StorageRoleCidata
}

// restorePatchStorageConfigs returns the disks to feed into patchCHConfig so
// the disk count matches the snapshot's config.json. When the snapshot did
// NOT carry cidata (cloudimg post-first-boot), ensureCloneCidata appended a
// fresh cidata entry — patchCHConfig must not see it (snapshot config.json
// has no slot for it; cidata is hot-plugged later).
//
// When the snapshot carried cidata, or for direct-boot/Windows VMs that
// never have cidata, we pass everything through unchanged.
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

// updateDataDiskPaths rewrites every Role==Data entry's Path to live under
// newRunDir, derived from the disk's serial. Used by clone after sidecar
// load: the sidecar carries the source VM's runDir, but the clone's data
// disk files have been reflinked into the clone's runDir.
func updateDataDiskPaths(configs []*types.StorageConfig, newRunDir string) {
	for _, sc := range configs {
		if sc.Role == types.StorageRoleData {
			sc.Path = filepath.Join(newRunDir, "data-"+sc.Serial+".raw")
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
	var cmdline strings.Builder
	fmt.Fprintf(&cmdline,
		"console=hvc0 loglevel=3 boot=cocoon-overlay cocoon.layers=%s cocoon.cow=%s clocksource=kvm-clock rw",
		strings.Join(ReverseLayerSerials(storageConfigs), ","), CowSerial,
	)

	if len(networkConfigs) > 0 {
		cmdline.WriteString(" net.ifnames=0")
		cmdline.WriteString(hypervisor.BuildIPParams(networkConfigs, vmName, dnsServers))
	}

	return cmdline.String()
}

// buildStateReplacements builds old→new string mappings for state.json patching.
// Only disk paths need patching (snapshot paths → clone paths). When
// ensureCloneCidata appended a fresh cidata entry, storageConfigs is one
// longer than chCfg.Disks; the prefix-min iteration covers the entries the
// snapshot already knew about, which is exactly what state.json references.
//
// MAC addresses are no longer patched here — hot-swap (vm.remove-device +
// vm.add-net) replaces the entire virtio-net device with the correct MAC.
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

// hotSwapNets removes old NICs (carrying stale MAC from snapshot binary state)
// and adds new ones with the correct MAC/TAP configuration.
// Must be called while VM is paused (between vm.restore and vm.resume).
func hotSwapNets(ctx context.Context, hc *http.Client, oldNets []chNet, networkConfigs []*types.NetworkConfig) error {
	logger := log.WithFunc("cloudhypervisor.hotSwapNets")
	for _, oldNet := range oldNets {
		if oldNet.ID == "" {
			continue
		}
		if err := removeDeviceVM(ctx, hc, oldNet.ID); err != nil {
			return fmt.Errorf("remove net device %s: %w", oldNet.ID, err)
		}
		logger.Infof(ctx, "removed snapshot NIC %s (old MAC %s)", oldNet.ID, oldNet.Mac)
	}
	for i, nc := range networkConfigs {
		newNet := networkConfigToNet(nc)
		if err := addNetVM(ctx, hc, newNet); err != nil {
			return fmt.Errorf("add net device %d/%d (MAC %s, TAP %s): %w",
				i+1, len(networkConfigs), nc.Mac, nc.Tap, err)
		}
		logger.Infof(ctx, "added NIC with MAC %s on TAP %s", nc.Mac, nc.Tap)
	}
	return nil
}
