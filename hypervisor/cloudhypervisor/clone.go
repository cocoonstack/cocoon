package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

// Clone creates a new VM from a snapshot tar stream.
// The snapshot's CH config.json is parsed to rebuild StorageConfigs, BootConfig,
// and ImageBlobIDs. The config is then patched with the new VM's paths, network,
// and resource settings before launching CH with vm.restore.
//
// Uses the same three-phase pattern as Create:
//
//	Phase 1: placeholder record (so GC won't orphan dirs)
//	Phase 2: extract + prepare files (parse, verify, patch, resize)
//	Phase 3: launch CH, restore, finalize record → Running
func (ch *CloudHypervisor) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, snapshotCfg *types.SnapshotConfig, networkConfigs []*types.NetworkConfig, snapshot io.Reader) (*types.VM, error) {
	logger := log.WithFunc("cloudhypervisor.Clone")

	now := time.Now()
	runDir := ch.conf.VMRunDir(vmID)
	logDir := ch.conf.VMLogDir(vmID)

	// Rollback on any failure after the placeholder is written.
	// All cleanup ops are idempotent — safe even if dirs/records don't exist yet.
	success := false
	defer func() {
		if !success {
			_ = removeVMDirs(runDir, logDir)
			ch.rollbackCreate(ctx, vmID, vmCfg.Name)
		}
	}()

	// Phase 1: write placeholder record so GC won't treat our dirs as orphans.
	// Uses snapshotCfg.ImageBlobIDs for GC pinning; full metadata is filled in
	// during finalize after parsing the snapshot's CH config.json.
	if err := ch.reserveVM(ctx, vmID, vmCfg, snapshotCfg.ImageBlobIDs, runDir, logDir); err != nil {
		return nil, fmt.Errorf("reserve VM record: %w", err)
	}

	// Phase 2: create directories and prepare snapshot files.
	if err := utils.EnsureDirs(runDir, logDir); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}

	// Extract snapshot tar into runDir.
	// Produces: config.json, state.json, memory-ranges, cow.raw or overlay.qcow2
	if err := utils.ExtractTar(runDir, snapshot); err != nil {
		return nil, fmt.Errorf("extract snapshot: %w", err)
	}

	// Parse CH config.json to rebuild VM metadata.
	chConfigPath := filepath.Join(runDir, "config.json")
	chCfg, err := parseCHConfig(chConfigPath)
	if err != nil {
		return nil, fmt.Errorf("parse CH config: %w", err)
	}

	storageConfigs := rebuildStorageConfigs(chCfg)
	bootCfg := rebuildBootConfig(chCfg)
	blobIDs := extractBlobIDs(storageConfigs, bootCfg)
	directBoot := isDirectBoot(bootCfg)

	// Update COW disk path to the new runDir.
	var cowPath string
	if directBoot {
		cowPath = ch.conf.COWRawPath(vmID)
	} else {
		cowPath = ch.conf.OverlayPath(vmID)
	}
	updateCOWPath(storageConfigs, cowPath, directBoot)

	// For cloudimg, update cidata path to the new runDir.
	// cidata.img is included in the snapshot tar and was extracted to runDir.
	if !directBoot {
		for _, sc := range storageConfigs {
			if sc.RO && isCidataDisk(sc) {
				sc.Path = filepath.Join(runDir, "cidata.img")
			}
		}
	}

	// Verify base layer files exist.
	if err := verifyBaseFiles(storageConfigs, bootCfg); err != nil {
		return nil, fmt.Errorf("verify base files: %w", err)
	}

	// Resize COW disk if user specified a larger --storage.
	if vmCfg.Storage > 0 {
		if err := resizeCOW(ctx, cowPath, vmCfg.Storage, directBoot); err != nil {
			return nil, fmt.Errorf("resize COW: %w", err)
		}
	}

	// Patch CH config.json with new paths, network, and resources.
	consoleSock := filepath.Join(runDir, "console.sock")
	if err := patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: storageConfigs,
		networkConfigs: networkConfigs,
		consoleSock:    consoleSock,
		cpu:            vmCfg.CPU,
		memory:         vmCfg.Memory,
	}); err != nil {
		return nil, fmt.Errorf("patch CH config: %w", err)
	}

	// Phase 3: launch CH, restore snapshot, finalize record.
	// Launch CH process with only --api-socket.
	sockPath := socketPath(runDir)
	args := []string{"--api-socket", sockPath}
	ch.saveCmdline(ctx, &hypervisor.VMRecord{RunDir: runDir}, args)

	withNetwork := len(networkConfigs) > 0
	pid, err := ch.launchProcess(ctx, &hypervisor.VMRecord{
		VM:             types.VM{NetworkConfigs: networkConfigs},
		RunDir:         runDir,
		LogDir:         logDir,
	}, sockPath, args, withNetwork)
	if err != nil {
		ch.markError(ctx, vmID)
		return nil, fmt.Errorf("launch CH: %w", err)
	}

	hc := utils.NewSocketHTTPClient(sockPath)

	// vm.restore + vm.resume
	if err := restoreVM(ctx, hc, runDir); err != nil {
		ch.abortLaunch(ctx, pid, sockPath, runDir)
		return nil, fmt.Errorf("vm.restore: %w", err)
	}
	if err := resumeVM(ctx, hc); err != nil {
		ch.abortLaunch(ctx, pid, sockPath, runDir)
		return nil, fmt.Errorf("vm.resume: %w", err)
	}

	// Finalize VMRecord → Running.
	// Console path is resolved lazily by Console() on first access.
	info := types.VM{
		ID: vmID, State: types.VMStateRunning,
		Config:         *vmCfg,
		StorageConfigs: storageConfigs,
		NetworkConfigs: networkConfigs,
		CreatedAt:      now, UpdatedAt: now,
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
		r.FirstBooted = true
		return nil
	}); err != nil {
		ch.abortLaunch(ctx, pid, sockPath, runDir)
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}

	success = true
	logger.Infof(ctx, "VM %s cloned from snapshot", vmID)
	return &info, nil
}

// parseCHConfig reads and deserializes the CH config.json from a snapshot directory.
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

// rebuildStorageConfigs reconstructs StorageConfigs from the CH config's disk list.
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

// rebuildBootConfig reconstructs BootConfig from the CH config's payload.
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

// verifyBaseFiles checks that all read-only base layer files and boot files exist.
func verifyBaseFiles(storageConfigs []*types.StorageConfig, boot *types.BootConfig) error {
	for _, sc := range storageConfigs {
		if !sc.RO {
			continue
		}
		if _, err := os.Stat(sc.Path); err != nil {
			return fmt.Errorf("base layer %s: %w", sc.Path, err)
		}
	}
	if boot != nil {
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
	}
	return nil
}

// updateCOWPath replaces the COW disk path in storageConfigs to point to the new runDir.
func updateCOWPath(configs []*types.StorageConfig, newCOWPath string, directBoot bool) {
	for _, sc := range configs {
		if sc.RO {
			continue
		}
		if directBoot {
			// OCI: the writable disk with serial "cocoon-cow"
			if sc.Serial == CowSerial {
				sc.Path = newCOWPath
			}
		} else {
			// cloudimg: the writable qcow2 overlay (no serial)
			sc.Path = newCOWPath
		}
	}
}

// resizeCOW enlarges the COW disk if the requested size exceeds its current logical size.
func resizeCOW(ctx context.Context, cowPath string, targetSize int64, directBoot bool) error {
	fi, err := os.Stat(cowPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", cowPath, err)
	}
	if targetSize <= fi.Size() {
		return nil // already large enough
	}

	if directBoot {
		// OCI raw: extend sparse file
		if err := os.Truncate(cowPath, targetSize); err != nil {
			return fmt.Errorf("truncate %s to %d: %w", cowPath, targetSize, err)
		}
	} else {
		// cloudimg qcow2: qemu-img resize
		sizeStr := fmt.Sprintf("%d", targetSize)
		if out, err := exec.CommandContext(ctx, //nolint:gosec
			"qemu-img", "resize", cowPath, sizeStr,
		).CombinedOutput(); err != nil {
			return fmt.Errorf("qemu-img resize %s: %s: %w", cowPath, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// patchOptions holds the parameters for patching a snapshot's CH config.json.
type patchOptions struct {
	storageConfigs []*types.StorageConfig
	networkConfigs []*types.NetworkConfig
	consoleSock    string
	cpu            int
	memory         int64
}

// patchCHConfig reads the CH config.json, patches disk paths, network, console,
// CPU, and memory, then writes it back.
func patchCHConfig(path string, opts *patchOptions) error {
	chCfg, err := parseCHConfig(path)
	if err != nil {
		return err
	}

	// Patch disks: update paths from new storageConfigs (matched by index).
	if len(opts.storageConfigs) != len(chCfg.Disks) {
		return fmt.Errorf("disk count mismatch: storageConfigs=%d, CH config=%d",
			len(opts.storageConfigs), len(chCfg.Disks))
	}
	for i, sc := range opts.storageConfigs {
		chCfg.Disks[i].Path = sc.Path
	}

	// Patch network: replace with new network configs.
	if len(opts.networkConfigs) > 0 {
		chCfg.Nets = make([]chNet, len(opts.networkConfigs))
		for i, nc := range opts.networkConfigs {
			chCfg.Nets[i] = networkConfigToNet(nc)
		}
	} else {
		chCfg.Nets = nil
	}

	// Patch console socket for UEFI boot (serial socket mode).
	if chCfg.Serial.Mode == "Socket" && opts.consoleSock != "" {
		chCfg.Serial.Socket = opts.consoleSock
	}

	// Patch CPU and memory.
	if opts.cpu > 0 {
		chCfg.CPUs.BootVCPUs = opts.cpu
	}
	if opts.memory > 0 {
		chCfg.Memory.Size = opts.memory
		// Recalculate balloon: 25% of memory when memory >= 256 MiB.
		if opts.memory >= minBalloonMemory {
			chCfg.Balloon = &chBalloon{
				Size:              opts.memory / 4, //nolint:mnd
				DeflateOnOOM:      true,
				FreePageReporting: true,
			}
		} else {
			chCfg.Balloon = nil
		}
	}

	// Write back.
	data, err := json.Marshal(chCfg)
	if err != nil {
		return fmt.Errorf("marshal patched config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
