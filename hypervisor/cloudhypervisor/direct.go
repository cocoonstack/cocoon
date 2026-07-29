package cloudhypervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

// DirectClone clones from a local snapshot dir. Per-type: hardlink memory-range-*, reflink/copy COW, plain copy metadata; cidata is regenerated.
func (ch *CloudHypervisor) DirectClone(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error) {
	// The copy step's parse feeds cloneAfterExtractParsed: config.json is copied verbatim, so re-parsing the runDir copy would decode identical bytes.
	var srcCfg *chVMConfig
	spec := hypervisor.CloneSpec{
		VMCfg: vmCfg, Net: net, SnapshotConfig: snapshotConfig,
		AfterExtract: func(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, runDir, logDir string, now time.Time, sourceSnapshotID string) (*types.VM, error) {
			return ch.cloneAfterExtractParsed(ctx, vmID, vmCfg, net, runDir, logDir, now, sourceSnapshotID, srcCfg)
		},
	}
	return ch.DirectCloneBase(ctx, vmID, spec, srcDir, func(dstDir, srcDir string) error {
		var err error
		srcCfg, err = cloneSnapshotFiles(ctx, dstDir, srcDir)
		return err
	})
}

// DirectRestore restores a VM in place from a local snapshot dir.
func (ch *CloudHypervisor) DirectRestore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, srcDir, sourceSnapshotID string) (*types.VM, error) {
	var meta *hypervisor.SnapshotMeta
	return ch.DirectRestoreSequence(ctx, vmRef, hypervisor.DirectRestoreSpec{
		VMCfg:            vmCfg,
		SrcDir:           srcDir,
		SourceSnapshotID: sourceSnapshotID,
		Preflight: func(srcDir string, rec *hypervisor.VMRecord) error {
			var err error
			meta, err = ch.preflightRestore(ctx, srcDir, rec, vmCfg)
			return err
		},
		Kill: ch.killForRestore,
		Populate: func(rec *hypervisor.VMRecord, srcDir string) error {
			return hypervisor.PopulateFromSrc(rec.RunDir, srcDir, cleanSnapshotFiles, func(dstDir, srcDir string) error {
				_, err := cloneSnapshotFiles(ctx, dstDir, srcDir)
				return err
			})
		},
		AfterExtract: func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord) (*types.VM, error) {
			directBoot := hypervisor.IsDirectBoot(rec.BootConfig)
			return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, directBoot, meta)
		},
	})
}

func cloneSnapshotFiles(ctx context.Context, dstDir, srcDir string) (*chVMConfig, error) {
	chCfg, err := parseCHConfig(filepath.Join(srcDir, configJSONName))
	if err != nil {
		return nil, fmt.Errorf("parse source config: %w", err)
	}
	cowFiles := identifyCOWFiles(chCfg)
	return chCfg, hypervisor.CloneSnapshotFiles(ctx, dstDir, srcDir, func(name string) hypervisor.SnapshotFileKind {
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
