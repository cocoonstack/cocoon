package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const cloneBackupSuffix = ".cocoon-clone-backup"

// driveRedirect records a temporary symlink replacing a source drive path.
type driveRedirect struct {
	symlinkPath string
	backupPath  string
	createdDir  bool
}

// Clone creates a new VM from a snapshot tar stream via FC snapshot/load.
func (fc *Firecracker) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, snapshotConfig *types.SnapshotConfig, snapshot io.Reader) (*types.VM, error) {
	return fc.CloneFromStream(ctx, vmID, vmCfg, networkConfigs, snapshotConfig, snapshot, fc.cloneAfterExtract)
}

// cloneAfterExtract contains all clone logic after snapshot data is in runDir.
// FC snapshots require the same directory layout — paths are absolute.
func (fc *Firecracker) cloneAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, runDir, logDir string, now time.Time) (*types.VM, error) {
	logger := log.WithFunc("firecracker.Clone")

	meta, err := loadSnapshotMeta(runDir, fc.conf.RootDir, fc.conf.Config.RunDir)
	if err != nil {
		return nil, fmt.Errorf("load snapshot metadata: %w", err)
	}

	// FC cannot update CPU/memory after snapshot/load. Reject overrides early.
	if meta.CPU > 0 && vmCfg.CPU != meta.CPU {
		return nil, fmt.Errorf("--cpu %d not supported: Firecracker cannot change CPU after snapshot/load (snapshot has %d)", vmCfg.CPU, meta.CPU)
	}
	if meta.Memory > 0 && vmCfg.Memory != meta.Memory {
		return nil, fmt.Errorf("--memory not supported: Firecracker cannot change memory after snapshot/load")
	}

	// Move extracted COW to canonical path.
	cowPath := fc.conf.COWRawPath(vmID)
	snapshotCOW := filepath.Join(runDir, cowFileName)
	if renameErr := os.Rename(snapshotCOW, cowPath); renameErr != nil {
		return nil, fmt.Errorf("move COW to canonical path: %w", renameErr)
	}

	// Rebuild storage: reuse RO layer paths from metadata, new COW path.
	storageConfigs := rebuildCloneStorage(meta, cowPath)
	bootCfg := meta.BootConfig
	if bootCfg != nil && bootCfg.KernelPath != "" {
		vmlinuxPath, extractErr := EnsureVmlinux(bootCfg.KernelPath)
		if extractErr != nil {
			return nil, fmt.Errorf("extract vmlinux for clone: %w", extractErr)
		}
		bootCfg.KernelPath = vmlinuxPath
	}
	blobIDs := hypervisor.ExtractBlobIDs(storageConfigs, bootCfg)

	if verifyErr := hypervisor.VerifyBaseFiles(storageConfigs, bootCfg); verifyErr != nil {
		return nil, fmt.Errorf("verify base files: %w", verifyErr)
	}
	if vmCfg.Storage > 0 {
		if expandErr := hypervisor.ExpandRawImage(cowPath, vmCfg.Storage); expandErr != nil {
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

	// FC snapshot/load requires drives at the same absolute paths as the source.
	// RO layers are shared blobs (same path). Only the COW path changed.
	// TODO: Replace symlink redirect with drive_overrides when FC PR #5774
	// (github.com/firecracker-microvm/firecracker/pull/5774) is merged.
	sockPath := hypervisor.SocketPath(runDir)
	withNetwork := len(networkConfigs) > 0
	var pid int
	if cloneErr := withSourceCOWLocked(meta.StorageConfigs, func() error {
		redirects, redirectErr := createDriveRedirects(meta.StorageConfigs, storageConfigs)
		if redirectErr != nil {
			return fmt.Errorf("drive redirect: %w", redirectErr)
		}
		defer cleanupDriveRedirects(redirects)

		var launchErr error
		pid, launchErr = fc.launchProcess(ctx, &hypervisor.VMRecord{
			VM:     types.VM{ID: vmID, NetworkConfigs: networkConfigs},
			RunDir: runDir,
			LogDir: logDir,
		}, sockPath, withNetwork)
		if launchErr != nil {
			return fmt.Errorf("launch FC: %w", launchErr)
		}

		return fc.restoreAndResumeClone(ctx, pid, sockPath, runDir, networkConfigs)
	}); cloneErr != nil {
		fc.MarkError(ctx, vmID)
		return nil, cloneErr
	}

	info := &types.VM{
		ID: vmID, Hypervisor: typ, State: types.VMStateRunning,
		Config: *vmCfg, StorageConfigs: storageConfigs, NetworkConfigs: networkConfigs,
		CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	if err := fc.FinalizeClone(ctx, vmID, info, bootCfg, blobIDs); err != nil {
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
) (err error) {
	defer func() {
		if err != nil {
			fc.AbortLaunch(ctx, pid, sockPath, runDir, runtimeFiles)
		}
	}()

	// Use network_overrides to provide clone's TAP devices during snapshot/load.
	// FC re-creates network devices from vmstate — overrides replace the
	// source TAP with the clone's TAP so FC opens the right device.
	netOverrides := buildNetworkOverrides(networkConfigs)
	// FC opens drive files during snapshot/load and holds the fds.
	// The symlink redirect ensures FC opens the clone's COW, not the source's.
	// No drive reconfiguration needed — fds survive symlink cleanup.
	if err = loadSnapshotFC(ctx, sockPath, runDir, netOverrides); err != nil {
		return fmt.Errorf("snapshot/load: %w", err)
	}
	hc := utils.NewSocketHTTPClient(sockPath)
	if err = resumeVM(ctx, hc); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

func rebuildCloneStorage(meta *snapshotMeta, cowPath string) []*types.StorageConfig {
	var configs []*types.StorageConfig
	for _, sc := range meta.StorageConfigs {
		if sc.RO {
			configs = append(configs, &types.StorageConfig{
				Path: sc.Path, RO: true, Serial: sc.Serial,
				Role: types.StorageRoleLayer,
			})
		}
	}
	configs = append(configs, &types.StorageConfig{
		Path: cowPath, RO: false, Serial: hypervisor.CowSerial,
		Role: types.StorageRoleCOW,
	})
	return configs
}

// createDriveRedirects creates temporary symlinks from source COW path to
// clone COW path so FC snapshot/load can find drives at expected locations.
// Only creates redirects for paths that actually differ (i.e., COW disk).
func createDriveRedirects(srcConfigs, dstConfigs []*types.StorageConfig) ([]driveRedirect, error) {
	var redirects []driveRedirect
	for i, src := range srcConfigs {
		if i >= len(dstConfigs) || src.Path == dstConfigs[i].Path {
			continue
		}
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

// withSourceCOWLocked runs fn while holding the source COW lock.
// Recovers stale symlink backups from crashed clones before proceeding.
func withSourceCOWLocked(srcConfigs []*types.StorageConfig, fn func() error) error {
	for _, sc := range srcConfigs {
		if !sc.RO {
			return withCOWPathLocked(sc.Path, func() error {
				recoverStaleBackup(sc.Path)
				return fn()
			})
		}
	}
	return fn() // no RW disk, run unlocked
}

// recoverStaleBackup restores a backup file left by a crashed clone.
// Caller must hold the COW lock.
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
			HostDevName: nc.Tap,
		})
	}
	return overrides
}
