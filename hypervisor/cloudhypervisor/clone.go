package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
func (ch *CloudHypervisor) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, snapshotCfg *types.SnapshotConfig, networkConfigs []*types.NetworkConfig, snapshot io.Reader) (*types.VM, error) {
	logger := log.WithFunc("cloudhypervisor.Clone")

	if vmCfg.Image == "" && snapshotCfg.Image != "" {
		vmCfg.Image = snapshotCfg.Image
	}

	now := time.Now()
	runDir := ch.conf.VMRunDir(vmID)
	logDir := ch.conf.VMLogDir(vmID)

	success := false
	defer func() {
		if !success {
			_ = removeVMDirs(runDir, logDir)
			ch.rollbackCreate(ctx, vmID, vmCfg.Name)
		}
	}()

	// Phase 1: placeholder record so GC won't orphan dirs.
	if err := ch.reserveVM(ctx, vmID, vmCfg, snapshotCfg.ImageBlobIDs, runDir, logDir); err != nil {
		return nil, fmt.Errorf("reserve VM record: %w", err)
	}

	// Phase 2: extract + prepare.
	if err := utils.EnsureDirs(runDir, logDir); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	if err := utils.ExtractTar(runDir, snapshot); err != nil {
		return nil, fmt.Errorf("extract snapshot: %w", err)
	}

	chConfigPath := filepath.Join(runDir, "config.json")
	chCfg, err := parseCHConfig(chConfigPath)
	if err != nil {
		return nil, fmt.Errorf("parse CH config: %w", err)
	}

	storageConfigs := rebuildStorageConfigs(chCfg)
	bootCfg := rebuildBootConfig(chCfg)
	blobIDs := extractBlobIDs(storageConfigs, bootCfg)
	directBoot := isDirectBoot(bootCfg)

	var cowPath string
	if directBoot {
		cowPath = ch.conf.COWRawPath(vmID)
	} else {
		cowPath = ch.conf.OverlayPath(vmID)
	}
	updateCOWPath(storageConfigs, cowPath, directBoot)

	// Update cidata path (cloudimg only; may be absent if snapshot was taken after restart).
	cidataPath := ch.conf.CidataPath(vmID)
	if !directBoot {
		for _, sc := range storageConfigs {
			if isCidataDisk(sc) {
				sc.Path = cidataPath
			}
		}
	}

	if err = verifyBaseFiles(storageConfigs, bootCfg); err != nil {
		return nil, fmt.Errorf("verify base files: %w", err)
	}
	if vmCfg.Storage > 0 {
		if err = resizeCOW(ctx, cowPath, vmCfg.Storage, directBoot); err != nil {
			return nil, fmt.Errorf("resize COW: %w", err)
		}
	}

	// Build old→new disk path mapping for state.json patching.
	diskPathMap := make(map[string]string, len(chCfg.Disks))
	for i, d := range chCfg.Disks {
		if storageConfigs[i].Path != d.Path {
			diskPathMap[d.Path] = storageConfigs[i].Path
		}
	}

	consoleSock := filepath.Join(runDir, "console.sock")
	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: storageConfigs,
		networkConfigs: networkConfigs,
		consoleSock:    consoleSock,
		directBoot:     directBoot,
		cpu:            vmCfg.CPU,
		memory:         vmCfg.Memory,
		vmName:         vmCfg.Name,
		dnsServers:     ch.conf.DNSServers(),
	}); err != nil {
		return nil, fmt.Errorf("patch CH config: %w", err)
	}

	// Patch state.json disk_path (informational only, prevents debugging confusion).
	stateJSONPath := filepath.Join(runDir, "state.json")
	if err = patchStateJSON(stateJSONPath, diskPathMap); err != nil {
		return nil, fmt.Errorf("patch state.json: %w", err)
	}

	// Update bootCfg.Cmdline for restarts (new VM name, IP, DNS).
	if directBoot && bootCfg != nil {
		bootCfg.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, ch.conf.DNSServers())
	}

	// Cloudimg: regenerate cidata with clone's identity and network config.
	if !directBoot {
		if err = ch.generateCidata(vmID, vmCfg, networkConfigs); err != nil {
			return nil, fmt.Errorf("generate cidata: %w", err)
		}
		// Ensure cidata is in storageConfigs (may be absent from snapshot).
		if !slices.ContainsFunc(storageConfigs, func(sc *types.StorageConfig) bool {
			return isCidataDisk(sc)
		}) {
			storageConfigs = append(storageConfigs, &types.StorageConfig{
				Path: cidataPath,
				RO:   true,
			})
		}
	}

	// Phase 3: launch CH, restore, finalize.
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

	hc := utils.NewSocketHTTPClient(sockPath)
	if err := restoreVM(ctx, hc, runDir); err != nil {
		ch.abortLaunch(ctx, pid, sockPath, runDir)
		return nil, fmt.Errorf("vm.restore: %w", err)
	}
	if err := resumeVM(ctx, hc); err != nil {
		ch.abortLaunch(ctx, pid, sockPath, runDir)
		return nil, fmt.Errorf("vm.resume: %w", err)
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
		// Cloudimg: FirstBooted=false → first restart attaches cidata → cloud-init re-runs.
		r.FirstBooted = directBoot
		return nil
	}); err != nil {
		ch.abortLaunch(ctx, pid, sockPath, runDir)
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}

	success = true
	logger.Infof(ctx, "VM %s cloned from snapshot", vmID)
	return &info, nil
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

func updateCOWPath(configs []*types.StorageConfig, newCOWPath string, directBoot bool) {
	for _, sc := range configs {
		if sc.RO {
			continue
		}
		if directBoot {
			if sc.Serial == CowSerial {
				sc.Path = newCOWPath
			}
		} else {
			sc.Path = newCOWPath
		}
	}
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

type patchOptions struct {
	storageConfigs []*types.StorageConfig
	networkConfigs []*types.NetworkConfig
	consoleSock    string
	directBoot     bool
	cpu            int
	memory         int64
	vmName         string
	dnsServers     []string
}

func patchCHConfig(path string, opts *patchOptions) error {
	chCfg, err := parseCHConfig(path)
	if err != nil {
		return err
	}

	// Disk paths.
	if len(opts.storageConfigs) != len(chCfg.Disks) {
		return fmt.Errorf("disk count mismatch: storageConfigs=%d, CH config=%d",
			len(opts.storageConfigs), len(chCfg.Disks))
	}
	for i, sc := range opts.storageConfigs {
		chCfg.Disks[i].Path = sc.Path
	}

	// Network: in-place update to preserve CH-assigned device IDs.
	if len(opts.networkConfigs) != len(chCfg.Nets) {
		return fmt.Errorf("net count mismatch: networkConfigs=%d, CH config=%d",
			len(opts.networkConfigs), len(chCfg.Nets))
	}
	for i, nc := range opts.networkConfigs {
		chCfg.Nets[i].Tap = nc.Tap
		chCfg.Nets[i].Mac = nc.Mac
		chCfg.Nets[i].NumQueues = netNumQueues(opts.cpu)
		chCfg.Nets[i].QueueSize = nc.QueueSize
		chCfg.Nets[i].OffloadTSO = true
		chCfg.Nets[i].OffloadUFO = true
		chCfg.Nets[i].OffloadCsum = true
	}

	// Serial/console: fresh config (snapshot carries stale /dev/pts/N paths).
	if opts.directBoot {
		chCfg.Serial = &chRuntimeFile{Mode: "Off"}
		chCfg.Console = &chRuntimeFile{Mode: "Pty"}
	} else {
		chCfg.Serial = &chRuntimeFile{Mode: "Socket", Socket: opts.consoleSock}
		chCfg.Console = &chRuntimeFile{Mode: "Off"}
	}

	// Kernel cmdline (OCI direct-boot only).
	if opts.directBoot && chCfg.Payload != nil {
		chCfg.Payload.Cmdline = buildCmdline(opts.storageConfigs, opts.networkConfigs, opts.vmName, opts.dnsServers)
	}

	// CPU and memory.
	if opts.cpu > 0 {
		chCfg.CPUs.BootVCPUs = opts.cpu
	}
	if opts.memory > 0 {
		chCfg.Memory.Size = opts.memory
		// Recalculate balloon but preserve device ID.
		if opts.memory >= minBalloonMemory {
			if chCfg.Balloon != nil {
				chCfg.Balloon.Size = opts.memory / 4 //nolint:mnd
			} else {
				chCfg.Balloon = &chBalloon{
					Size:              opts.memory / 4, //nolint:mnd
					DeflateOnOOM:      true,
					FreePageReporting: true,
				}
			}
		} else {
			chCfg.Balloon = nil
		}
	}

	data, err := json.Marshal(chCfg)
	if err != nil {
		return fmt.Errorf("marshal patched config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
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
		dns0, dns1 := dnsFromConfig(dnsServers)
		for i, n := range networkConfigs {
			if n.Network == nil || n.Network.IP == "" {
				continue
			}
			fmt.Fprintf(&cmdline, " ip=%s::%s:%s:%s:eth%d:off:%s:%s",
				n.Network.IP, n.Network.Gateway,
				prefixToNetmask(n.Network.Prefix), vmName, i, dns0, dns1)
		}
	}

	return cmdline.String()
}

// patchStateJSON updates stale disk_path in state.json (informational only;
// CH opens disks via config.json, but stale paths cause debugging confusion).
func patchStateJSON(path string, diskPathMap map[string]string) error {
	if len(diskPathMap) == 0 {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content := string(data)
	for oldPath, newPath := range diskPathMap {
		content = strings.ReplaceAll(content, oldPath, newPath)
	}
	return os.WriteFile(path, []byte(content), 0o600) //nolint:gosec
}
