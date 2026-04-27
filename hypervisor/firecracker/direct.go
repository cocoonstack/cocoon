package firecracker

import (
	"context"
	"fmt"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

// DirectClone creates a new VM from a local snapshot directory.
// Files are handled per-type: hardlink for mem, reflink/copy for
// the COW disk, plain copy for small metadata (vmstate).
func (fc *Firecracker) DirectClone(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error) {
	return fc.DirectCloneBase(ctx, vmID, vmCfg, networkConfigs, snapshotConfig, srcDir, cloneSnapshotFiles, fc.cloneAfterExtract)
}

// DirectRestore reverts a running VM using a local snapshot directory.
// Files are handled per-type: hardlink for mem, reflink/copy for
// the COW disk, plain copy for small metadata (vmstate).
func (fc *Firecracker) DirectRestore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, srcDir string) (*types.VM, error) {
	if err := hypervisor.ValidateHostCPU(vmCfg.CPU); err != nil {
		return nil, err
	}
	// Validate CPU/memory overrides before killing the running VM.
	if checkErr := fc.validateRestoreOverrides(ctx, vmRef, vmCfg); checkErr != nil {
		return nil, checkErr
	}

	vmID, rec, cowPath, err := fc.prepareRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	var result *types.VM
	if lockErr := withCOWPathLocked(cowPath, func() error {
		if cleanErr := cleanSnapshotFiles(rec.RunDir); cleanErr != nil {
			fc.MarkError(ctx, vmID)
			return fmt.Errorf("clean old snapshot files: %w", cleanErr)
		}
		if cloneErr := cloneSnapshotFiles(rec.RunDir, srcDir); cloneErr != nil {
			fc.MarkError(ctx, vmID)
			return fmt.Errorf("clone snapshot files: %w", cloneErr)
		}
		var restoreErr error
		result, restoreErr = fc.restoreAfterExtract(ctx, vmID, vmCfg, rec, cowPath)
		return restoreErr
	}); lockErr != nil {
		return nil, lockErr
	}
	return result, nil
}

// cloneSnapshotFiles copies snapshot files from srcDir to dstDir using
// per-file strategies: hardlink/symlink for mem, reflink/sparse for COW,
// plain copy for metadata (vmstate).
func cloneSnapshotFiles(dstDir, srcDir string) error {
	return hypervisor.CloneSnapshotFiles(dstDir, srcDir, func(name string) hypervisor.SnapshotFileKind {
		switch {
		case name == snapshotMemFile:
			return hypervisor.SnapshotFileMemory
		case strings.HasSuffix(name, ".raw"):
			return hypervisor.SnapshotFileCOW
		default:
			return hypervisor.SnapshotFileMeta
		}
	})
}

// cleanSnapshotFiles removes snapshot-specific files from runDir.
// Data disks (data-<name>.raw) are listed explicitly so stale survivors
// don't linger across restore.
func cleanSnapshotFiles(runDir string) error {
	return hypervisor.CleanSnapshotFiles(runDir, func(name string) bool {
		switch name {
		case snapshotVMStateFile, snapshotMemFile, cowFileName, snapshotMetaFile:
			return true
		}
		if strings.HasPrefix(name, "data-") && strings.HasSuffix(name, ".raw") {
			return true
		}
		return false
	})
}
