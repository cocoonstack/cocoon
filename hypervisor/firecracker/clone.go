package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const cloneBackupSuffix = ".cocoon-clone-backup"

type driveRedirect struct {
	symlinkPath string
	backupPath  string
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
		releaseDirs, holdErr := holdRedirectDirs(ctx, meta.StorageConfigs, storageConfigs)
		if holdErr != nil {
			return holdErr
		}
		defer releaseDirs()
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
	// Re-anchor redirected drives at the clone's own paths: the loaded vmstate still names the source's, and a future snapshot would embed those dangling paths, breaking its restore.
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

// redirectedDriveIndices lists drives whose source and clone paths differ — the one source of truth for both createDriveRedirects and the re-anchor loop, which must never diverge.
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
	}
}

// holdRedirectDirs recreates missing parents of redirected source paths and holds their ops locks: the dirs are recordless, and the GC orphan scan reaps any unlocked recordless dir mid-clone.
func holdRedirectDirs(ctx context.Context, srcConfigs, dstConfigs []*types.StorageConfig) (func(), error) {
	var dirs []string
	seen := make(map[string]struct{})
	for _, i := range redirectedDriveIndices(srcConfigs, dstConfigs) {
		dir := filepath.Dir(srcConfigs[i].Path)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	locks := make([]*flock.Lock, 0, len(dirs))
	release := func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = locks[i].Unlock(ctx)
			_ = os.Remove(dirs[i])
		}
	}
	for _, dir := range dirs {
		l, err := lockRecreatedDir(ctx, dir)
		if err != nil {
			release()
			return nil, err
		}
		locks = append(locks, l)
	}
	return release, nil
}

// lockRecreatedDir mkdirs+locks with retry: a concurrent GC collect can remove the still-empty dir between MkdirAll and the flock open.
func lockRecreatedDir(ctx context.Context, dir string) (*flock.Lock, error) {
	for {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("recreate source dir: %w", err)
		}
		l := flock.NewTransient(filepath.Join(dir, hypervisor.OpsLockName))
		switch err := l.Lock(ctx); {
		case err == nil:
			return l, nil
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
}

// recoverStaleBackup clears a crashed clone's redirect symlink and restores its backup; caller must hold the COW lock. Imported metadata paths are untrusted: a symlink is removed only with a backup beside it or when it sits inside the managed run root, where every cocoon redirect lives and nothing foreign does.
func recoverStaleBackup(runRoot, cowPath string) {
	backup := cowPath + cloneBackupSuffix
	_, backupErr := os.Lstat(backup)
	if fi, err := os.Lstat(cowPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if backupErr == nil || hypervisor.IsUnderDir(cowPath, runRoot) {
			_ = os.Remove(cowPath)
		}
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
