package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

// Clone creates a new VM from a snapshot tar stream via vm.restore.
// Three phases: placeholder record → extract+prepare → launch+finalize.
func (ch *CloudHypervisor) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, snapshotConfig *types.SnapshotConfig, snapshot io.Reader) (_ *types.VM, err error) {
	if vmCfg.Image == "" && snapshotConfig.Image != "" {
		vmCfg.Image = snapshotConfig.Image
	}

	now := time.Now()
	runDir := ch.conf.VMRunDir(vmID)
	logDir := ch.conf.VMLogDir(vmID)

	defer func() {
		if err != nil {
			_ = removeVMDirs(runDir, logDir)
			ch.rollbackCreate(ctx, vmID, vmCfg.Name)
		}
	}()

	if err = ch.reserveVM(ctx, vmID, vmCfg, snapshotConfig.ImageBlobIDs, runDir, logDir); err != nil {
		return nil, fmt.Errorf("reserve VM record: %w", err)
	}
	if err = utils.EnsureDirs(runDir, logDir); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	if err = utils.ExtractTar(runDir, snapshot); err != nil {
		return nil, fmt.Errorf("extract snapshot: %w", err)
	}

	return ch.cloneAfterExtract(ctx, vmID, vmCfg, networkConfigs, runDir, logDir, now)
}

// cloneAfterExtract contains all clone logic after snapshot data is in runDir.
// Shared by Clone (tar stream) and DirectClone (direct file copy).
func (ch *CloudHypervisor) cloneAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, runDir, logDir string, now time.Time) (*types.VM, error) {
	logger := log.WithFunc("cloudhypervisor.Clone")

	chConfigPath := filepath.Join(runDir, "config.json")
	chCfg, err := parseCHConfig(chConfigPath)
	if err != nil {
		return nil, fmt.Errorf("parse CH config: %w", err)
	}

	storageConfigs := rebuildStorageConfigs(chCfg)
	bootCfg := rebuildBootConfig(chCfg)
	blobIDs := extractBlobIDs(storageConfigs, bootCfg)
	directBoot := isDirectBoot(bootCfg)

	cowPath := ch.cowPath(vmID, directBoot)
	if err = updateCOWPath(storageConfigs, cowPath, directBoot); err != nil {
		return nil, fmt.Errorf("update COW path: %w", err)
	}

	// Update cidata path (cloudimg only; may be absent if snapshot was taken after restart).
	hadCidataInSnapshot := updateCloneCidataPath(storageConfigs, directBoot, ch.conf.CidataPath(vmID))

	if err = verifyBaseFiles(storageConfigs, bootCfg); err != nil {
		return nil, fmt.Errorf("verify base files: %w", err)
	}
	if vmCfg.Storage > 0 {
		if err = resizeCOW(ctx, cowPath, vmCfg.Storage, directBoot); err != nil {
			return nil, fmt.Errorf("resize COW: %w", err)
		}
	}

	stateReplacements := buildStateReplacements(chCfg, storageConfigs)

	// Cloudimg: regenerate cidata with clone's identity and network config.
	storageConfigs, err = ch.ensureCloneCidata(vmID, vmCfg, networkConfigs, storageConfigs, directBoot)
	if err != nil {
		return nil, err
	}

	// vm.restore requires config/state device tree equality.
	// If snapshot had no cidata disk, patch only snapshot disks and hotplug cidata later.
	patchStorageConfigs := restorePatchStorageConfigs(storageConfigs, directBoot, hadCidataInSnapshot)

	consoleSock := consoleSockPath(runDir)
	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: patchStorageConfigs,
		consoleSock:    consoleSock,
		directBoot:     directBoot,
		cpu:            vmCfg.CPU,
		memory:         vmCfg.Memory,
	}); err != nil {
		return nil, fmt.Errorf("patch CH config: %w", err)
	}

	// Patch state.json: disk paths (informational, prevents debugging confusion).
	stateJSONPath := filepath.Join(runDir, "state.json")
	if err = patchStateJSON(stateJSONPath, stateReplacements); err != nil {
		return nil, fmt.Errorf("patch state.json: %w", err)
	}

	// Update bootCfg.Cmdline for restarts (new VM name, IP, DNS).
	if directBoot && bootCfg != nil {
		bootCfg.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, ch.conf.DNSServers())
	}

	// Launch CH, restore, finalize.
	sockPath := socketPath(runDir)
	args := []string{"--api-socket", sockPath}
	ch.saveCmdline(ctx, &hypervisor.VMRecord{RunDir: runDir}, args)

	withNetwork := len(networkConfigs) > 0
	pid, err := ch.launchProcess(ctx, &hypervisor.VMRecord{
		VM:     types.VM{NetworkConfigs: networkConfigs},
		RunDir: runDir,
		LogDir: logDir,
	}, sockPath, args, withNetwork)
	if err != nil {
		ch.markError(ctx, vmID)
		return nil, fmt.Errorf("launch CH: %w", err)
	}

	if err := ch.restoreAndResumeClone(ctx, pid, sockPath, runDir, directBoot, hadCidataInSnapshot, storageConfigs, networkConfigs, chCfg, vmCfg.CPU); err != nil {
		return nil, err
	}

	// Finalize record → Running.
	info := types.VM{
		ID:             vmID,
		State:          types.VMStateRunning,
		Config:         *vmCfg,
		StorageConfigs: storageConfigs,
		NetworkConfigs: networkConfigs,
		CreatedAt:      now,
		UpdatedAt:      now,
		StartedAt:      &now,
	}
	if err := ch.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
		r := idx.VMs[vmID]
		if r == nil {
			return fmt.Errorf("VM %s disappeared from index", vmID)
		}
		r.VM = info
		r.BootConfig = bootCfg
		r.ImageBlobIDs = blobIDs
		// Clone VM is already running with cidata attached; cloud-init reinit
		// is done via post-clone hints. Mark as first-booted so the next
		// cold boot (stop+start) skips cidata — no need for a second cloud-init run.
		r.FirstBooted = true
		return nil
	}); err != nil {
		ch.abortLaunch(ctx, pid, sockPath, runDir)
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}

	logger.Infof(ctx, "VM %s cloned from snapshot", vmID)
	return &info, nil
}

func (ch *CloudHypervisor) restoreAndResumeClone(
	ctx context.Context,
	pid int,
	sockPath, runDir string,
	directBoot, hadCidataInSnapshot bool,
	storageConfigs []*types.StorageConfig,
	networkConfigs []*types.NetworkConfig,
	snapshotCfg *chVMConfig,
	cpu int,
) (err error) {
	defer func() {
		if err != nil {
			ch.abortLaunch(ctx, pid, sockPath, runDir)
		}
	}()

	hc := utils.NewSocketHTTPClient(sockPath)
	if err = restoreVM(ctx, hc, runDir); err != nil {
		return fmt.Errorf("vm.restore: %w", err)
	}

	// Hot-swap NICs while paused: remove snapshot's virtio-net devices (which carry
	// the old MAC baked in binary device state), then add fresh ones with correct MAC.
	// The guest will discover the new devices via ACPI GED notification on resume.
	if err = hotSwapNets(ctx, hc, snapshotCfg.Nets, networkConfigs); err != nil {
		return fmt.Errorf("hot-swap NICs: %w", err)
	}

	if !directBoot && !hadCidataInSnapshot {
		if len(storageConfigs) == 0 {
			return fmt.Errorf("vm.add-disk (cidata): missing storage config")
		}
		cidataDisk := storageConfigToDisk(storageConfigs[len(storageConfigs)-1], cpu)
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
	if directBoot {
		return storageConfigs, nil
	}
	if err := ch.generateCidata(vmID, vmCfg, networkConfigs); err != nil {
		return nil, fmt.Errorf("generate cidata: %w", err)
	}
	cidataPath := ch.conf.CidataPath(vmID)
	// Keep cidata in VM record for future starts; snapshot may not carry it.
	if !slices.ContainsFunc(storageConfigs, isCidataDisk) {
		storageConfigs = append(storageConfigs, &types.StorageConfig{
			Path: cidataPath,
			RO:   true,
		})
	}
	return storageConfigs, nil
}

func parseCHConfig(path string) (*chVMConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg chVMConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &cfg, nil
}

func rebuildStorageConfigs(cfg *chVMConfig) []*types.StorageConfig {
	var configs []*types.StorageConfig
	for _, d := range cfg.Disks {
		configs = append(configs, &types.StorageConfig{
			Path:   d.Path,
			RO:     d.ReadOnly,
			Serial: d.Serial,
		})
	}
	return configs
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
		if isCidataDisk(sc) {
			sc.Path = cidataPath
			hadCidataInSnapshot = true
		}
	}
	return hadCidataInSnapshot
}

func restorePatchStorageConfigs(storageConfigs []*types.StorageConfig, directBoot, hadCidataInSnapshot bool) []*types.StorageConfig {
	if directBoot || hadCidataInSnapshot || len(storageConfigs) == 0 {
		return storageConfigs
	}
	return storageConfigs[:len(storageConfigs)-1]
}

func verifyBaseFiles(storageConfigs []*types.StorageConfig, boot *types.BootConfig) error {
	for _, sc := range storageConfigs {
		if !sc.RO {
			continue
		}
		if _, err := os.Stat(sc.Path); err != nil {
			return fmt.Errorf("base layer %s: %w", sc.Path, err)
		}
	}
	if boot == nil {
		return nil
	}
	if boot.KernelPath != "" {
		if _, err := os.Stat(boot.KernelPath); err != nil {
			return fmt.Errorf("kernel %s: %w", boot.KernelPath, err)
		}
	}
	if boot.InitrdPath != "" {
		if _, err := os.Stat(boot.InitrdPath); err != nil {
			return fmt.Errorf("initrd %s: %w", boot.InitrdPath, err)
		}
	}
	if boot.FirmwarePath != "" {
		if _, err := os.Stat(boot.FirmwarePath); err != nil {
			return fmt.Errorf("firmware %s: %w", boot.FirmwarePath, err)
		}
	}
	return nil
}

func updateCOWPath(configs []*types.StorageConfig, newCOWPath string, directBoot bool) error {
	if directBoot {
		found := false
		for _, sc := range configs {
			if !sc.RO && sc.Serial == CowSerial {
				sc.Path = newCOWPath
				found = true
			}
		}
		if !found {
			return fmt.Errorf("no writable disk with serial %q found", CowSerial)
		}
		return nil
	}
	for _, sc := range configs {
		if !sc.RO {
			sc.Path = newCOWPath
		}
	}
	return nil
}

func resizeCOW(ctx context.Context, cowPath string, targetSize int64, directBoot bool) error {
	fi, err := os.Stat(cowPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", cowPath, err)
	}
	if targetSize <= fi.Size() {
		return nil // already large enough
	}

	if directBoot {
		if err := os.Truncate(cowPath, targetSize); err != nil {
			return fmt.Errorf("truncate %s to %d: %w", cowPath, targetSize, err)
		}
	} else {
		sizeStr := fmt.Sprintf("%d", targetSize)
		if out, err := exec.CommandContext(ctx, //nolint:gosec
			"qemu-img", "resize", cowPath, sizeStr,
		).CombinedOutput(); err != nil {
			return fmt.Errorf("qemu-img resize %s: %s: %w", cowPath, strings.TrimSpace(string(out)), err)
		}
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
		cmdline.WriteString(buildIPParams(networkConfigs, vmName, dnsServers))
	}

	return cmdline.String()
}

// buildIPParams generates kernel ip= parameters for all NICs with static IPs.
// If no NIC has a static IP (DHCP mode), emits a hostname-only ip= param so
// the initramfs still sets /etc/hostname via /run/net-*.conf.
func buildIPParams(networkConfigs []*types.NetworkConfig, vmName string, dnsServers []string) string {
	dns0, dns1 := dnsFromConfig(dnsServers)
	var params strings.Builder
	for i, n := range networkConfigs {
		if n.Network == nil || n.Network.IP == "" {
			continue
		}
		param := fmt.Sprintf(" ip=%s::%s:%s:%s:eth%d:off",
			n.Network.IP, n.Network.Gateway,
			prefixToNetmask(n.Network.Prefix), vmName, i)
		if dns0 != "" {
			param += ":" + dns0
			if dns1 != "" {
				param += ":" + dns1
			}
		}
		params.WriteString(param)
	}
	if params.Len() == 0 {
		fmt.Fprintf(&params, " ip=::::%s::off", vmName)
	}
	return params.String()
}

// buildStateReplacements builds old→new string mappings for state.json patching.
// Only disk paths need patching (snapshot paths → clone paths).
// MAC addresses are no longer patched here — hot-swap (vm.remove-device + vm.add-net)
// replaces the entire virtio-net device with the correct MAC.
func buildStateReplacements(chCfg *chVMConfig, storageConfigs []*types.StorageConfig) map[string]string {
	m := make(map[string]string, len(chCfg.Disks))
	if len(storageConfigs) == len(chCfg.Disks) {
		for i, d := range chCfg.Disks {
			if storageConfigs[i].Path != d.Path {
				m[d.Path] = storageConfigs[i].Path
			}
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
	for _, nc := range networkConfigs {
		newNet := networkConfigToNet(nc)
		if err := addNetVM(ctx, hc, newNet); err != nil {
			return fmt.Errorf("add net device for %s: %w", nc.Mac, err)
		}
		logger.Infof(ctx, "added NIC with MAC %s on TAP %s", nc.Mac, nc.Tap)
	}
	return nil
}

// prefixToNetmask converts a CIDR prefix length to a dotted-decimal netmask string.
func prefixToNetmask(prefix int) string {
	mask := net.CIDRMask(prefix, 32)
	return net.IP(mask).String()
}

// dnsFromConfig returns the first two DNS servers for kernel ip= param.
func dnsFromConfig(servers []string) (string, string) {
	dns0, dns1 := "", ""
	if len(servers) > 0 {
		dns0 = servers[0]
	}
	if len(servers) > 1 {
		dns1 = servers[1]
	}
	return dns0, dns1
}
