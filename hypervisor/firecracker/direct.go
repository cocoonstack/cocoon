package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// DirectClone creates a new VM from a local snapshot directory.
// Files are handled per-type: hardlink for mem, reflink/copy for
// the COW disk, plain copy for small metadata (vmstate).
func (fc *Firecracker) DirectClone(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, snapshotConfig *types.SnapshotConfig, srcDir string) (_ *types.VM, err error) {
	runDir, logDir, now, cleanup, err := fc.cloneSetup(ctx, vmID, vmCfg, snapshotConfig)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	if err = cloneSnapshotFiles(runDir, srcDir); err != nil {
		return nil, fmt.Errorf("clone snapshot files: %w", err)
	}

	return fc.cloneAfterExtract(ctx, vmID, vmCfg, networkConfigs, runDir, logDir, now)
}

// DirectRestore reverts a running VM using a local snapshot directory.
// Files are handled per-type: hardlink for mem, reflink/copy for
// the COW disk, plain copy for small metadata (vmstate).
func (fc *Firecracker) DirectRestore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, srcDir string) (*types.VM, error) {
	// Validate CPU/memory overrides before killing the running VM.
	if checkErr := fc.validateRestoreOverrides(ctx, vmRef, vmCfg); checkErr != nil {
		return nil, checkErr
	}

	vmID, rec, cowPath, err := fc.prepareRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	// Serialize with concurrent clone redirects that may symlink cowPath.
	cowUnlock, cowLockErr := lockCOWPath(cowPath)
	if cowLockErr != nil {
		return nil, fmt.Errorf("lock COW: %w", cowLockErr)
	}
	defer cowUnlock()

	// Clean old snapshot files from runDir before linking/copying new ones.
	if cleanErr := cleanSnapshotFiles(rec.RunDir); cleanErr != nil {
		fc.MarkError(ctx, vmID)
		return nil, fmt.Errorf("clean old snapshot files: %w", cleanErr)
	}

	if cloneErr := cloneSnapshotFiles(rec.RunDir, srcDir); cloneErr != nil {
		fc.MarkError(ctx, vmID)
		return nil, fmt.Errorf("clone snapshot files: %w", cloneErr)
	}

	return fc.restoreAfterExtract(ctx, vmID, vmCfg, rec, cowPath)
}

// cloneSnapshotFiles copies snapshot files from srcDir to dstDir using
// per-file strategies to minimize I/O:
//   - mem: hardlink (FC memory file is not modified after load)
//   - cow.raw: ReflinkCopy (FICLONE -> SparseCopy, the only real copy)
//   - everything else: plain copy (vmstate -- small metadata)
func cloneSnapshotFiles(dstDir, srcDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read srcDir: %w", err)
	}

	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		src := filepath.Join(srcDir, name)
		dst := filepath.Join(dstDir, name)

		switch {
		case name == snapshotMemFile:
			// Memory file: hardlink (FC uses MAP_PRIVATE, file is not modified).
			if err := os.Link(src, dst); err != nil {
				return fmt.Errorf("hardlink %s: %w", name, err)
			}
		case strings.HasSuffix(name, ".raw"):
			// COW disk: reflink copy.
			if err := utils.ReflinkCopy(dst, src); err != nil {
				return fmt.Errorf("reflink copy %s: %w", name, err)
			}
		default:
			// Small metadata (vmstate): plain copy.
			if err := hypervisor.CopyFile(dst, src); err != nil {
				return fmt.Errorf("copy %s: %w", name, err)
			}
		}
	}
	return nil
}

// cleanSnapshotFiles removes snapshot-specific files (mem, vmstate, cow.raw)
// from runDir before replacing them with new snapshot data.
func cleanSnapshotFiles(runDir string) error {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		// Keep files that are NOT part of snapshot data (e.g., logs, PID file).
		if name == snapshotVMStateFile || name == snapshotMemFile || name == cowFileName {
			if removeErr := os.Remove(filepath.Join(runDir, name)); removeErr != nil {
				return fmt.Errorf("remove %s: %w", name, removeErr)
			}
		}
	}
	return nil
}
