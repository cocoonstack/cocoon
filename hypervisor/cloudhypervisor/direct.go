package cloudhypervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

// DirectClone clones from a local snapshot dir. Per-type: hardlink memory-range-*, reflink/copy COW, plain copy metadata; cidata is regenerated.
func (ch *CloudHypervisor) DirectClone(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error) {
	return ch.DirectCloneBase(ctx, vmID, vmCfg, net, snapshotConfig, srcDir, cloneSnapshotFiles, ch.cloneAfterExtract)
}

// DirectRestore restores a VM in place from a local snapshot dir.
func (ch *CloudHypervisor) DirectRestore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, srcDir, sourceSnapshotID string) (*types.VM, error) {
	return ch.DirectRestoreSequence(ctx, vmRef, hypervisor.DirectRestoreSpec{
		VMCfg:            vmCfg,
		SrcDir:           srcDir,
		SourceSnapshotID: sourceSnapshotID,
		Preflight:        ch.preflightRestore,
		Kill:             ch.killForRestore,
		Populate: func(rec *hypervisor.VMRecord, srcDir string) error {
			return hypervisor.PopulateFromSrc(rec.RunDir, srcDir, cleanSnapshotFiles, cloneSnapshotFiles)
		},
		AfterExtract: func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord) (*types.VM, error) {
			directBoot := isDirectBoot(rec.BootConfig)
			return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, directBoot)
		},
	})
}

func cloneSnapshotFiles(dstDir, srcDir string) error {
	chCfg, err := parseCHConfig(filepath.Join(srcDir, configJSONName))
	if err != nil {
		return fmt.Errorf("parse source config: %w", err)
	}
	cowFiles := identifyCOWFiles(chCfg)
	return hypervisor.CloneSnapshotFiles(dstDir, srcDir, func(name string) hypervisor.SnapshotFileKind {
		switch {
		case strings.HasPrefix(name, memoryRangeFile):
			return hypervisor.SnapshotFileMemory
		case cowFiles[name]:
			return hypervisor.SnapshotFileCOW
		default:
			return hypervisor.SnapshotFileMeta
		}
	})
}

// cleanSnapshotFiles enumerates by name so stale data-*.raw and cocoon.json from a previous incarnation don't linger; COW files are overwritten anyway.
func cleanSnapshotFiles(runDir string) error {
	return hypervisor.CleanSnapshotFiles(runDir, func(name string) bool {
		switch {
		case strings.HasPrefix(name, memoryRangeFile):
			return true
		case name == configJSONName || name == stateJSONName:
			return true
		case name == hypervisor.SnapshotMetaFile:
			return true
		case hypervisor.IsDataDiskFile(name):
			return true
		}
		return false
	})
}

func identifyCOWFiles(cfg *chVMConfig) map[string]bool {
	m := make(map[string]bool)
	for _, d := range cfg.Disks {
		if !d.ReadOnly {
			m[filepath.Base(d.Path)] = true
		}
	}
	return m
}
