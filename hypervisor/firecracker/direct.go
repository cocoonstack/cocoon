package firecracker

import (
	"context"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

// DirectClone clones from a local snapshot dir. Per-type: hardlink mem, reflink/copy COW, plain copy metadata.
func (fc *Firecracker) DirectClone(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error) {
	return fc.DirectCloneBase(ctx, vmID, vmCfg, net, snapshotConfig, srcDir, cloneSnapshotFiles, fc.cloneAfterExtract)
}

// DirectRestore restores a VM in place from a local snapshot dir.
func (fc *Firecracker) DirectRestore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, srcDir, sourceSnapshotID string) (*types.VM, error) {
	return fc.DirectRestoreSequence(ctx, vmRef, hypervisor.DirectRestoreSpec{
		VMCfg:            vmCfg,
		SrcDir:           srcDir,
		SourceSnapshotID: sourceSnapshotID,
		Preflight:        fc.preflightRestore,
		Kill:             fc.killForRestore,
		Wrap:             fc.wrapSourceLocked,
		Populate: func(rec *hypervisor.VMRecord, srcDir string) error {
			return hypervisor.PopulateFromSrc(rec.RunDir, srcDir, cleanSnapshotFiles, cloneSnapshotFiles)
		},
		AfterExtract: fc.restoreAfterExtractCOW,
	})
}

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

// cleanSnapshotFiles enumerates by name so stale data-*.raw don't linger across restore.
func cleanSnapshotFiles(runDir string) error {
	return hypervisor.CleanSnapshotFiles(runDir, func(name string) bool {
		switch name {
		case snapshotVMStateFile, snapshotMemFile, hypervisor.COWRawFileName, hypervisor.SnapshotMetaFile:
			return true
		}
		return hypervisor.IsDataDiskFile(name)
	})
}
