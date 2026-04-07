package firecracker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Clone creates a new VM from a snapshot tar stream via FC snapshot/load.
// Three phases: placeholder record -> extract+prepare -> launch+finalize.
func (fc *Firecracker) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, snapshotConfig *types.SnapshotConfig, snapshot io.Reader) (_ *types.VM, err error) {
	runDir, logDir, now, cleanup, err := fc.cloneSetup(ctx, vmID, vmCfg, snapshotConfig)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	if err = utils.ExtractTar(runDir, snapshot); err != nil {
		return nil, fmt.Errorf("extract snapshot: %w", err)
	}

	return fc.cloneAfterExtract(ctx, vmID, vmCfg, networkConfigs, runDir, logDir, now)
}

func (fc *Firecracker) cloneSetup(ctx context.Context, vmID string, vmCfg *types.VMConfig, snapshotConfig *types.SnapshotConfig) (runDir, logDir string, now time.Time, cleanup func(), err error) {
	if vmCfg.Image == "" && snapshotConfig.Image != "" {
		vmCfg.Image = snapshotConfig.Image
	}

	now = time.Now()
	runDir = fc.conf.VMRunDir(vmID)
	logDir = fc.conf.VMLogDir(vmID)

	cleanup = func() {
		_ = removeVMDirs(runDir, logDir)
		fc.rollbackCreate(ctx, vmID, vmCfg.Name)
	}

	if err = fc.reserveVM(ctx, vmID, vmCfg, snapshotConfig.ImageBlobIDs, runDir, logDir); err != nil {
		return "", "", time.Time{}, nil, fmt.Errorf("reserve VM record: %w", err)
	}
	if err = utils.EnsureDirs(runDir, logDir); err != nil {
		cleanup()
		return "", "", time.Time{}, nil, fmt.Errorf("ensure dirs: %w", err)
	}
	return runDir, logDir, now, cleanup, nil
}

// cloneAfterExtract contains all clone logic after snapshot data is in runDir.
// Shared by Clone (tar stream) and DirectClone (direct file copy).
func (fc *Firecracker) cloneAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, runDir, logDir string, now time.Time) (*types.VM, error) {
	logger := log.WithFunc("firecracker.Clone")

	// Rebuild storage/boot configs from the snapshot's cow.raw and existing record context.
	// FC stores StorageConfigs on the VMRecord (not in a config.json like CH).
	// We need to find the COW file in the snapshot and update paths.
	cowPath := fc.conf.COWRawPath(vmID)
	snapshotCOW := filepath.Join(runDir, "cow.raw")

	// Move the extracted COW to its canonical location.
	if err := os.Rename(snapshotCOW, cowPath); err != nil {
		return nil, fmt.Errorf("move COW to canonical path: %w", err)
	}

	// Rebuild storage configs: read-only layers from snapshot config (via blob IDs),
	// plus the new COW disk.
	storageConfigs, bootCfg, blobIDs, err := fc.rebuildFromSnapshot(ctx, vmID, vmCfg, cowPath)
	if err != nil {
		return nil, fmt.Errorf("rebuild from snapshot: %w", err)
	}

	if verifyErr := verifyBaseFiles(storageConfigs, bootCfg); verifyErr != nil {
		return nil, fmt.Errorf("verify base files: %w", verifyErr)
	}

	// Expand COW if vmCfg requests larger storage.
	if vmCfg.Storage > 0 {
		if expandErr := expandRawImage(cowPath, vmCfg.Storage); expandErr != nil {
			return nil, fmt.Errorf("resize COW: %w", expandErr)
		}
	}

	// Update bootCfg.Cmdline for the new clone (new VM name, IP, DNS).
	if bootCfg != nil {
		dns, dnsErr := fc.conf.DNSServers()
		if dnsErr != nil {
			return nil, fmt.Errorf("parse DNS servers: %w", dnsErr)
		}
		bootCfg.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, dns)
	}

	// Launch FC process, load snapshot, configure drives, resume.
	sockPath := socketPath(runDir)

	withNetwork := len(networkConfigs) > 0
	pid, err := fc.launchProcess(ctx, &hypervisor.VMRecord{
		VM:     types.VM{NetworkConfigs: networkConfigs},
		RunDir: runDir,
		LogDir: logDir,
	}, sockPath, withNetwork)
	if err != nil {
		fc.markError(ctx, vmID)
		return nil, fmt.Errorf("launch FC: %w", err)
	}

	if err := fc.restoreAndResumeClone(ctx, pid, sockPath, runDir, storageConfigs, networkConfigs); err != nil {
		return nil, err
	}

	// Finalize record -> Running.
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
	if err := fc.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
		r := idx.VMs[vmID]
		if r == nil {
			return fmt.Errorf("vm %s disappeared from index", vmID)
		}
		r.VM = info
		r.BootConfig = bootCfg
		r.ImageBlobIDs = blobIDs
		r.FirstBooted = true
		return nil
	}); err != nil {
		fc.abortLaunch(ctx, pid, sockPath, runDir)
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}

	logger.Infof(ctx, "VM %s cloned from snapshot", vmID)
	return &info, nil
}

func (fc *Firecracker) restoreAndResumeClone(
	ctx context.Context,
	pid int,
	sockPath, runDir string,
	storageConfigs []*types.StorageConfig,
	networkConfigs []*types.NetworkConfig,
) (err error) {
	defer func() {
		if err != nil {
			fc.abortLaunch(ctx, pid, sockPath, runDir)
		}
	}()

	hc := utils.NewSocketHTTPClient(sockPath)
	if err = loadSnapshotFC(ctx, hc, runDir); err != nil {
		return fmt.Errorf("snapshot/load: %w", err)
	}

	// Re-configure drives after snapshot load.
	// FC snapshot/load does NOT preserve drive config; drives must be re-attached.
	if err = fc.reconfigureDrives(ctx, hc, storageConfigs); err != nil {
		return fmt.Errorf("reconfigure drives: %w", err)
	}

	// Re-configure network interfaces for the clone (new TAP devices, new MACs).
	if err = fc.reconfigureNetworks(ctx, hc, networkConfigs); err != nil {
		return fmt.Errorf("reconfigure networks: %w", err)
	}

	if err = resumeVM(ctx, hc); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

// rebuildFromSnapshot reconstructs StorageConfigs, BootConfig, and blob IDs
// from the VM's image (looked up via vmCfg.Image) plus the new COW path.
// FC only supports OCI (direct boot), so we always have a kernel+initrd+layers.
func (fc *Firecracker) rebuildFromSnapshot(ctx context.Context, _ string, vmCfg *types.VMConfig, cowPath string) ([]*types.StorageConfig, *types.BootConfig, map[string]struct{}, error) {
	// Look up the original VM that was snapshotted to find its storage layout.
	// For clone, the snapshot already carried the COW; we need the read-only layers
	// which are shared blobs on disk (referenced by the image).
	// The caller (cmd layer) already resolved the image and passed storageConfigs
	// via snapshotConfig.ImageBlobIDs. We reconstruct from the index.

	// Search for any existing VM with the same image to get layer paths.
	// This is a fallback; the primary path is through the image resolver at the cmd layer.
	var storageConfigs []*types.StorageConfig
	var bootCfg *types.BootConfig

	if err := fc.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		for _, rec := range idx.VMs {
			if rec == nil || rec.Config.Image != vmCfg.Image {
				continue
			}
			// Found a VM with the same image; reuse its read-only layers and boot config.
			for _, sc := range rec.StorageConfigs {
				if sc.RO {
					storageConfigs = append(storageConfigs, &types.StorageConfig{
						Path:   sc.Path,
						RO:     true,
						Serial: sc.Serial,
					})
				}
			}
			if rec.BootConfig != nil {
				b := *rec.BootConfig
				bootCfg = &b
			}
			return nil
		}
		return fmt.Errorf("no VM with image %q found for layer reference", vmCfg.Image)
	}); err != nil {
		return nil, nil, nil, err
	}

	// Append the new COW disk.
	storageConfigs = append(storageConfigs, &types.StorageConfig{
		Path:   cowPath,
		RO:     false,
		Serial: CowSerial,
	})

	blobIDs := extractBlobIDs(storageConfigs, bootCfg)
	return storageConfigs, bootCfg, blobIDs, nil
}

// reconfigureDrives re-attaches drives after FC snapshot/load.
// FC does not preserve drive configuration across snapshot/load boundaries.
func (fc *Firecracker) reconfigureDrives(ctx context.Context, hc *http.Client, storageConfigs []*types.StorageConfig) error {
	for i, sc := range storageConfigs {
		driveID := fmt.Sprintf("drive_%d", i)
		if err := putDrive(ctx, hc, fcDrive{
			DriveID:      driveID,
			PathOnHost:   sc.Path,
			IsRootDevice: false,
			IsReadOnly:   sc.RO,
		}); err != nil {
			return fmt.Errorf("drive %s: %w", driveID, err)
		}
	}
	return nil
}

// reconfigureNetworks re-attaches network interfaces after FC snapshot/load.
// Clone VMs get new TAP devices and MACs.
func (fc *Firecracker) reconfigureNetworks(ctx context.Context, hc *http.Client, networkConfigs []*types.NetworkConfig) error {
	for i, nc := range networkConfigs {
		ifaceID := fmt.Sprintf("eth%d", i)
		if err := putNetworkInterface(ctx, hc, fcNetworkInterface{
			IfaceID:     ifaceID,
			HostDevName: nc.Tap,
			GuestMAC:    nc.Mac,
		}); err != nil {
			return fmt.Errorf("network-interface %s: %w", ifaceID, err)
		}
	}
	return nil
}

// verifyBaseFiles checks that all read-only layer files and boot files exist on disk.
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
	for _, check := range []struct{ name, path string }{
		{"kernel", boot.KernelPath},
		{"initrd", boot.InitrdPath},
	} {
		if check.path == "" {
			continue
		}
		if _, err := os.Stat(check.path); err != nil {
			return fmt.Errorf("%s %s: %w", check.name, check.path, err)
		}
	}
	return nil
}

// expandRawImage expands a raw disk image to targetSize if its current size is smaller.
func expandRawImage(path string, targetSize int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if targetSize <= fi.Size() {
		return nil
	}
	if err := os.Truncate(path, targetSize); err != nil {
		return fmt.Errorf("truncate %s to %d: %w", path, targetSize, err)
	}
	return nil
}
