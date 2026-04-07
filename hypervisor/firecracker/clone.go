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
		_ = hypervisor.RemoveVMDirs(runDir, logDir)
		fc.RollbackCreate(ctx, vmID, vmCfg.Name)
	}

	if err = fc.ReserveVM(ctx, vmID, vmCfg, snapshotConfig.ImageBlobIDs, runDir, logDir); err != nil {
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

	// Read snapshot metadata (cocoon.json) to reconstruct storage/boot config.
	// This makes the clone self-contained — no dependency on live VM records.
	meta, err := loadSnapshotMeta(runDir, fc.conf.RootDir)
	if err != nil {
		return nil, fmt.Errorf("load snapshot metadata: %w", err)
	}

	cowPath := fc.conf.COWRawPath(vmID)
	snapshotCOW := filepath.Join(runDir, cowFileName)
	if renameErr := os.Rename(snapshotCOW, cowPath); renameErr != nil {
		return nil, fmt.Errorf("move COW to canonical path: %w", renameErr)
	}

	// Rebuild storage configs: reuse layer paths from metadata, update COW path.
	storageConfigs := rebuildCloneStorage(meta, cowPath)
	bootCfg := meta.BootConfig
	// Metadata stores portable vmlinuz path; extract vmlinux for FC on this host.
	if bootCfg != nil && bootCfg.KernelPath != "" {
		vmlinuxPath, extractErr := ensureVmlinux(bootCfg.KernelPath)
		if extractErr != nil {
			return nil, fmt.Errorf("extract vmlinux for clone: %w", extractErr)
		}
		bootCfg.KernelPath = vmlinuxPath
	}
	blobIDs := hypervisor.ExtractBlobIDs(storageConfigs, bootCfg)

	if verifyErr := verifyBaseFiles(storageConfigs, bootCfg); verifyErr != nil {
		return nil, fmt.Errorf("verify base files: %w", verifyErr)
	}

	if vmCfg.Storage > 0 {
		if expandErr := expandRawImage(cowPath, vmCfg.Storage); expandErr != nil {
			return nil, fmt.Errorf("resize COW: %w", expandErr)
		}
	}

	if bootCfg != nil {
		dns, dnsErr := fc.conf.DNSServers()
		if dnsErr != nil {
			return nil, fmt.Errorf("parse DNS servers: %w", dnsErr)
		}
		bootCfg.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, dns)
	}

	// FC snapshot/load requires drives at the same paths baked into vmstate.
	// Use vmstatePaths (original absolute paths from source host) as symlink targets.
	// Serialize with other operations on the source COW via flock.
	vmstateSC := meta.vmstatePaths()
	unlock, lockErr := acquireCOWLock(vmstateSC)
	if lockErr != nil {
		return nil, fmt.Errorf("lock source COW: %w", lockErr)
	}
	defer unlock()
	redirects, redirectErr := createDriveRedirects(vmstateSC, storageConfigs)
	if redirectErr != nil {
		return nil, fmt.Errorf("drive redirect: %w", redirectErr)
	}
	defer cleanupDriveRedirects(redirects)

	sockPath := hypervisor.SocketPath(runDir)
	withNetwork := len(networkConfigs) > 0
	pid, err := fc.launchProcess(ctx, &hypervisor.VMRecord{
		VM:     types.VM{NetworkConfigs: networkConfigs},
		RunDir: runDir,
		LogDir: logDir,
	}, sockPath, withNetwork)
	if err != nil {
		fc.MarkError(ctx, vmID)
		return nil, fmt.Errorf("launch FC: %w", err)
	}

	if err := fc.restoreAndResumeClone(ctx, pid, sockPath, runDir, storageConfigs, networkConfigs); err != nil {
		return nil, err
	}

	// FC cannot update CPU/memory after snapshot/load (no PATCH /machine-config
	// on a restored VM). Use the snapshot's original values.
	if meta.CPU > 0 {
		vmCfg.CPU = meta.CPU
	}
	if meta.Memory > 0 {
		vmCfg.Memory = meta.Memory
	}

	info := types.VM{
		ID: vmID, State: types.VMStateRunning,
		Config: *vmCfg, StorageConfigs: storageConfigs, NetworkConfigs: networkConfigs,
		CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	if err := fc.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
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
		fc.AbortLaunch(ctx, pid, sockPath, runDir, runtimeFiles)
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
			fc.AbortLaunch(ctx, pid, sockPath, runDir, runtimeFiles)
		}
	}()

	hc := utils.NewSocketHTTPClient(sockPath)
	if err = loadSnapshotFC(ctx, hc, runDir); err != nil {
		return fmt.Errorf("snapshot/load: %w", err)
	}

	if err = fc.reconfigureDrives(ctx, hc, storageConfigs); err != nil {
		return fmt.Errorf("reconfigure drives: %w", err)
	}
	if err = fc.reconfigureNetworks(ctx, hc, networkConfigs); err != nil {
		return fmt.Errorf("reconfigure networks: %w", err)
	}

	if err = resumeVM(ctx, hc); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

// rebuildCloneStorage creates new StorageConfigs from snapshot metadata,
// keeping read-only layer paths unchanged and updating the COW path.
func rebuildCloneStorage(meta *snapshotMeta, cowPath string) []*types.StorageConfig {
	var configs []*types.StorageConfig
	for _, sc := range meta.StorageConfigs {
		if sc.RO {
			configs = append(configs, &types.StorageConfig{Path: sc.Path, RO: true, Serial: sc.Serial})
		}
	}
	configs = append(configs, &types.StorageConfig{Path: cowPath, RO: false, Serial: CowSerial})
	return configs
}

// driveRedirect records a temporary symlink replacing a source drive path
// with a pointer to the clone's actual file.
type driveRedirect struct {
	symlinkPath string // where the symlink was created (= source drive path)
	backupPath  string // non-empty if an existing file was renamed out of the way
	createdDir  bool   // true if the parent directory was created
}

// createDriveRedirects ensures FC snapshot/load opens the clone's drive files
// instead of the source VM's. For each drive path that changed (COW disk),
// it renames any existing file at the source path out of the way, then creates
// a symlink from source path → clone path. Returns error if a redirect cannot
// be installed (e.g., symlink fails after backup rename), to prevent clone from
// proceeding with corrupted source VM state.
func createDriveRedirects(srcConfigs, dstConfigs []*types.StorageConfig) ([]driveRedirect, error) {
	var redirects []driveRedirect
	for i, src := range srcConfigs {
		if i >= len(dstConfigs) || src.Path == dstConfigs[i].Path {
			continue
		}
		r := driveRedirect{symlinkPath: src.Path}

		// If the source file exists (source VM still alive), rename it
		// to a temporary backup so we can place a symlink at that path.
		if _, err := os.Stat(src.Path); err == nil {
			backup := src.Path + ".cocoon-clone-backup"
			if renameErr := os.Rename(src.Path, backup); renameErr != nil {
				cleanupDriveRedirects(redirects)
				return nil, fmt.Errorf("backup source drive %s: %w", src.Path, renameErr)
			}
			r.backupPath = backup
		}

		// Ensure parent directory exists (source VM may have been deleted).
		if _, err := os.Stat(filepath.Dir(src.Path)); err != nil {
			if mkErr := os.MkdirAll(filepath.Dir(src.Path), 0o700); mkErr != nil {
				cleanupDriveRedirects(redirects)
				return nil, fmt.Errorf("create dir for drive redirect %s: %w", src.Path, mkErr)
			}
			r.createdDir = true
		}

		if linkErr := os.Symlink(dstConfigs[i].Path, src.Path); linkErr != nil {
			// Restore backup before aborting so the source VM is not damaged.
			if r.backupPath != "" {
				_ = os.Rename(r.backupPath, src.Path)
			}
			cleanupDriveRedirects(redirects)
			return nil, fmt.Errorf("symlink drive redirect %s → %s: %w", src.Path, dstConfigs[i].Path, linkErr)
		}
		redirects = append(redirects, r)
	}
	return redirects, nil
}

// cleanupDriveRedirects removes the temporary symlinks and restores any
// backed-up source files.
// acquireCOWLock locks the source VM's writable COW disk path.
// Returns a no-op unlock if no writable drive is found.
func acquireCOWLock(srcConfigs []*types.StorageConfig) (func(), error) {
	for _, sc := range srcConfigs {
		if !sc.RO {
			return lockCOWPath(sc.Path)
		}
	}
	return func() {}, nil
}

func cleanupDriveRedirects(redirects []driveRedirect) {
	for _, r := range redirects {
		_ = os.Remove(r.symlinkPath)
		if r.backupPath != "" {
			_ = os.Rename(r.backupPath, r.symlinkPath)
		}
		if r.createdDir {
			_ = os.Remove(filepath.Dir(r.symlinkPath))
		}
	}
}

// reconfigureDrives re-attaches drives after FC snapshot/load.
// FC does not preserve drive configuration across snapshot/load boundaries.
func (fc *Firecracker) reconfigureDrives(ctx context.Context, hc *http.Client, storageConfigs []*types.StorageConfig) error {
	for i, sc := range storageConfigs {
		driveID := fmt.Sprintf(driveIDFmt, i)
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
		ifaceID := fmt.Sprintf(ifaceIDFmt, i)
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
