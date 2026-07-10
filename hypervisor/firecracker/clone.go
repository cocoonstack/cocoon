package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const cloneBackupSuffix = ".cocoon-clone-backup"

type driveRedirect struct {
	symlinkPath string
	backupPath  string
	createdDir  bool
}

func (fc *Firecracker) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, snapshot io.Reader) (*types.VM, error) {
	return fc.CloneFromStream(ctx, vmID, vmCfg, net, snapshotConfig, snapshot, fc.cloneAfterExtract)
}

func (fc *Firecracker) cloneAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, runDir, logDir string, now time.Time, sourceSnapshotID string) (*types.VM, error) {
	if len(vmCfg.DataDisks) > 0 {
		return nil, fmt.Errorf("--data-disk on clone is Cloud Hypervisor only (Firecracker has no disk hotplug): %w", disk.ErrUnsupportedBackend)
	}
	networkConfigs := net.NetworkConfigs
	logger := log.WithFunc("firecracker.Clone")

	meta, err := fc.conf.LoadAndValidateMeta(runDir)
	if err != nil {
		return nil, fmt.Errorf("load snapshot metadata: %w", err)
	}

	cowPath := fc.conf.COWRawPath(vmID)
	snapshotCOW := filepath.Join(runDir, hypervisor.COWRawFileName)
	if renameErr := os.Rename(snapshotCOW, cowPath); renameErr != nil {
		return nil, fmt.Errorf("move COW to canonical path: %w", renameErr)
	}

	storageConfigs, err := rebuildCloneStorage(meta, cowPath)
	if err != nil {
		return nil, err
	}
	if err := types.ValidateStorageConfigs(storageConfigs); err != nil {
		return nil, fmt.Errorf("validate sidecar: %w", err)
	}
	bootCfg := meta.BootConfig
	if err := EnsureVmlinuxBoot(bootCfg); err != nil {
		return nil, err
	}
	blobIDs := hypervisor.ExtractBlobIDs(storageConfigs, bootCfg)

	if verifyErr := hypervisor.VerifyBaseFiles(storageConfigs, bootCfg); verifyErr != nil {
		return nil, fmt.Errorf("verify base files: %w", verifyErr)
	}
	if err := fc.setBootCmdline(bootCfg, storageConfigs, networkConfigs, vmCfg.Name); err != nil {
		return nil, err
	}

	// FC snapshot/load wants source-absolute drive paths; symlink-redirect the source COW.
	sockPath := hypervisor.SocketPath(runDir)
	var pid int
	if cloneErr := fc.withSourceWritableDisksLocked(ctx, meta.StorageConfigs, func() error {
		redirects, redirectErr := createDriveRedirects(meta.StorageConfigs, storageConfigs)
		if redirectErr != nil {
			return fmt.Errorf("drive redirect: %w", redirectErr)
		}
		defer cleanupDriveRedirects(redirects)

		var launchErr error
		pid, launchErr = fc.launchProcess(ctx, &hypervisor.VMRecord{
			VM:     types.VM{ID: vmID},
			RunDir: runDir,
			LogDir: logDir,
		}, sockPath, net.NetnsPath)
		if launchErr != nil {
			return fmt.Errorf("launch FC: %w", launchErr)
		}

		return fc.restoreAndResumeClone(ctx, pid, sockPath, runDir, networkConfigs, meta.StorageConfigs, storageConfigs)
	}); cloneErr != nil {
		fc.MarkError(ctx, vmID)
		return nil, cloneErr
	}

	info := &types.VM{
		ID: vmID, Hypervisor: typ, State: types.VMStateRunning,
		Config: *vmCfg, StorageConfigs: storageConfigs,
		NetSetup:  net,
		CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	hypervisor.SetRunningSockets(info, runDir)
	if err := fc.FinalizeClone(ctx, vmID, info, bootCfg, blobIDs, sourceSnapshotID); err != nil {
		fc.AbortLaunch(ctx, pid, sockPath, runDir, runtimeFiles)
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}

	logger.Infof(ctx, "VM %s cloned from snapshot", vmID)
	return info, nil
}

func (fc *Firecracker) restoreAndResumeClone(
	ctx context.Context,
	pid int,
	sockPath, runDir string,
	networkConfigs []*types.NetworkConfig,
	srcConfigs, dstConfigs []*types.StorageConfig,
) (err error) {
	defer func() {
		if err != nil {
			fc.AbortLaunch(ctx, pid, sockPath, runDir, runtimeFiles)
		}
	}()

	// network_overrides repoints FC at the clone's TAP; vsock_override retargets the snapshot UDS.
	netOverrides := buildNetworkOverrides(networkConfigs)
	if err = loadSnapshotFC(ctx, sockPath, runDir, netOverrides, hypervisor.VsockSockPath(runDir)); err != nil {
		return fmt.Errorf("snapshot/load: %w", err)
	}
	hc := utils.NewSocketHTTPClient(sockPath)
	if err = resumeVM(ctx, hc); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	// Re-anchor redirected drives at the clone's own paths: the loaded
	// vmstate still names the source's, and any future snapshot of this VM
	// would embed those dangling paths — breaking its restore (hibernate).
	for _, i := range redirectedDriveIndices(srcConfigs, dstConfigs) {
		if err = patchDrivePath(ctx, hc, fmt.Sprintf(driveIDFmt, i), dstConfigs[i].Path); err != nil {
			return fmt.Errorf("re-anchor drive %d: %w", i, err)
		}
	}
	return nil
}

// rebuildCloneStorage rewrites paths per role (Layer→source, COW→cowPath, Data→runDir); cidata rejected.
func rebuildCloneStorage(meta *hypervisor.SnapshotMeta, cowPath string) ([]*types.StorageConfig, error) {
	runDir := filepath.Dir(cowPath)
	configs := hypervisor.CloneStorageConfigs(meta.StorageConfigs)
	for i, sc := range configs {
		switch sc.Role {
		case types.StorageRoleLayer:
		case types.StorageRoleCOW:
			sc.Path = cowPath
		case types.StorageRoleData:
			sc.Path = filepath.Join(runDir, hypervisor.DataDiskBaseName(sc.Serial))
		case types.StorageRoleCidata:
			return nil, fmt.Errorf("snapshot disk[%d] has cidata role; FC does not support cloudimg", i)
		default:
			return nil, fmt.Errorf("snapshot disk[%d] has unknown role %q", i, sc.Role)
		}
	}
	return configs, nil
}

// redirectedDriveIndices lists the drives whose source and clone paths
// differ: exactly the set createDriveRedirects symlinks and the re-anchor
// loop patches — the two must never diverge, so both derive from here.
func redirectedDriveIndices(srcConfigs, dstConfigs []*types.StorageConfig) []int {
	var indices []int
	for i, src := range srcConfigs {
		if i < len(dstConfigs) && src.Path != dstConfigs[i].Path {
			indices = append(indices, i)
		}
	}
	return indices
}

func createDriveRedirects(srcConfigs, dstConfigs []*types.StorageConfig) ([]driveRedirect, error) {
	var redirects []driveRedirect
	for _, i := range redirectedDriveIndices(srcConfigs, dstConfigs) {
		src := srcConfigs[i]
		r := driveRedirect{symlinkPath: src.Path}

		if _, err := os.Stat(src.Path); err == nil {
			backup := src.Path + cloneBackupSuffix
			if renameErr := os.Rename(src.Path, backup); renameErr != nil {
				cleanupDriveRedirects(redirects)
				return nil, fmt.Errorf("backup source drive %s: %w", src.Path, renameErr)
			}
			r.backupPath = backup
		}

		if _, err := os.Stat(filepath.Dir(src.Path)); err != nil {
			if mkErr := os.MkdirAll(filepath.Dir(src.Path), 0o700); mkErr != nil {
				cleanupDriveRedirects(redirects)
				return nil, fmt.Errorf("create dir for drive redirect %s: %w", src.Path, mkErr)
			}
			r.createdDir = true
		}

		if linkErr := os.Symlink(dstConfigs[i].Path, src.Path); linkErr != nil {
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

// recoverStaleBackup restores a crashed-clone backup; caller must hold the COW lock.
func recoverStaleBackup(cowPath string) {
	backup := cowPath + cloneBackupSuffix
	if _, err := os.Stat(backup); err != nil {
		return
	}
	fi, err := os.Lstat(cowPath)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(cowPath)
	}
	_ = os.Rename(backup, cowPath)
}

func buildNetworkOverrides(networkConfigs []*types.NetworkConfig) []fcNetworkOverride {
	var overrides []fcNetworkOverride
	for i, nc := range networkConfigs {
		overrides = append(overrides, fcNetworkOverride{
			IfaceID:     fmt.Sprintf(ifaceIDFmt, i),
			HostDevName: nc.TAP,
		})
	}
	return overrides
}
