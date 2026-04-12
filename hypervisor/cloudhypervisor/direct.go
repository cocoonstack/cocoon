package cloudhypervisor

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
// Files are handled per-type: hardlink for memory-range-*, reflink/copy for
// the COW disk, plain copy for small metadata, and cidata is regenerated.
func (ch *CloudHypervisor) DirectClone(ctx context.Context, vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, snapshotConfig *types.SnapshotConfig, srcDir string) (_ *types.VM, err error) {
	runDir, logDir, now, cleanup, err := ch.cloneSetup(ctx, vmID, vmCfg, snapshotConfig)
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

	return ch.cloneAfterExtract(ctx, vmID, vmCfg, networkConfigs, runDir, logDir, now)
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
// per-file strategies to minimize I/O:
//   - memory-range-*: hardlink (CH uses MAP_PRIVATE, files are not modified)
//   - COW disk: ReflinkCopy (FICLONE → SparseCopy, the only real copy)
//   - everything else: plain copy (config.json, state.json — small metadata)
func cloneSnapshotFiles(dstDir, srcDir string) error {
	chCfg, _, err := parseCHConfig(filepath.Join(srcDir, "config.json"))
	if err != nil {
		return fmt.Errorf("parse source config: %w", err)
	}
	cowFiles := identifyCOWFiles(chCfg)

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
		case strings.HasPrefix(name, "memory-range"):
			if err := os.Link(src, dst); err != nil {
				return fmt.Errorf("hardlink %s: %w", name, err)
			}
		case cowFiles[name]:
			if err := utils.ReflinkCopy(dst, src); err != nil {
				return fmt.Errorf("reflink copy %s: %w", name, err)
			}
		default:
			if err := hypervisor.CopyFile(dst, src); err != nil {
				return fmt.Errorf("copy %s: %w", name, err)
			}
		}
	}
	return nil
}

// cleanSnapshotFiles removes snapshot-specific files (memory-range-*, state.json,
// config.json, COW disk) from runDir before replacing them with new snapshot data.
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
		// Keep files that are NOT part of snapshot data (e.g., cidata, logs, cmdline).
		if strings.HasPrefix(name, "memory-range") ||
			name == "config.json" || name == "state.json" {
			if removeErr := os.Remove(filepath.Join(runDir, name)); removeErr != nil {
				return fmt.Errorf("remove %s: %w", name, removeErr)
			}
		}
	}
	// COW file may have arbitrary name; it's overwritten by cloneSnapshotFiles.
	return nil
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
