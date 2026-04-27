package cloudhypervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

// DirectClone creates a new VM from a local snapshot directory.
// Files are handled per-type: hardlink for memory-range-*, reflink/copy for
// the COW disk, plain copy for small metadata, and cidata is regenerated.
func (ch *CloudHypervisor) DirectClone(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error) {
	return ch.DirectCloneBase(ctx, vmID, vmCfg, networkConfigs, snapshotConfig, srcDir, cloneSnapshotFiles, ch.cloneAfterExtract)
}

// DirectRestore reverts a running VM using a local snapshot directory.
// Files are handled per-type: hardlink for memory-range-*, reflink/copy for
// the COW disk, plain copy for small metadata.
func (ch *CloudHypervisor) DirectRestore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, srcDir string) (*types.VM, error) {
	// Validate CPU BEFORE prepareRestore kills the running VM —
	// otherwise a bad --cpu override would tear down the VM only to
	// reject it afterwards.
	if err := hypervisor.ValidateHostCPU(vmCfg.CPU); err != nil {
		return nil, err
	}

	vmID, rec, directBoot, cowPath, err := ch.prepareRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	// Clean old snapshot files from runDir before linking/copying new ones.
	if cleanErr := cleanSnapshotFiles(rec.RunDir); cleanErr != nil {
		ch.MarkError(ctx, vmID)
		return nil, fmt.Errorf("clean old snapshot files: %w", cleanErr)
	}

	if cloneErr := cloneSnapshotFiles(rec.RunDir, srcDir); cloneErr != nil {
		ch.MarkError(ctx, vmID)
		return nil, fmt.Errorf("clone snapshot files: %w", cloneErr)
	}

	return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, directBoot, cowPath)
}

// cloneSnapshotFiles copies snapshot files from srcDir to dstDir using
// per-file strategies: hardlink/symlink for memory-range-*, reflink/sparse
// for COW disks, plain copy for metadata (config.json, state.json).
func cloneSnapshotFiles(dstDir, srcDir string) error {
	chCfg, _, err := parseCHConfig(filepath.Join(srcDir, "config.json"))
	if err != nil {
		return fmt.Errorf("parse source config: %w", err)
	}
	cowFiles := identifyCOWFiles(chCfg)
	return hypervisor.CloneSnapshotFiles(dstDir, srcDir, func(name string) hypervisor.SnapshotFileKind {
		switch {
		case strings.HasPrefix(name, "memory-range"):
			return hypervisor.SnapshotFileMemory
		case cowFiles[name]:
			return hypervisor.SnapshotFileCOW
		default:
			return hypervisor.SnapshotFileMeta
		}
	})
}

// cleanSnapshotFiles removes snapshot-specific files from runDir.
// COW files have arbitrary names and are overwritten by cloneSnapshotFiles;
// data disks (data-<name>.raw) and the cocoon.json sidecar are listed
// explicitly so stale survivors from a previous incarnation don't linger.
func cleanSnapshotFiles(runDir string) error {
	return hypervisor.CleanSnapshotFiles(runDir, func(name string) bool {
		switch {
		case strings.HasPrefix(name, "memory-range"):
			return true
		case name == "config.json" || name == "state.json":
			return true
		case name == snapshotMetaFile:
			return true
		case strings.HasPrefix(name, "data-") && strings.HasSuffix(name, ".raw"):
			return true
		}
		return false
	})
}

// identifyCOWFiles returns the set of basenames of writable (COW) disk files.
func identifyCOWFiles(cfg *chVMConfig) map[string]bool {
	m := make(map[string]bool)
	for _, d := range cfg.Disks {
		if !d.ReadOnly {
			m[filepath.Base(d.Path)] = true
		}
	}
	return m
}
