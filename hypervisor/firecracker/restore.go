package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (fc *Firecracker) Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, snapshot io.Reader, sourceSnapshotID string) (*types.VM, error) {
	return fc.RestoreSequence(ctx, vmRef, hypervisor.RestoreSpec{
		VMCfg:            vmCfg,
		Snapshot:         snapshot,
		SourceSnapshotID: sourceSnapshotID,
		Preflight:        fc.preflightRestore,
		Kill:             fc.killForRestore,
		// Same sweep as DirectRestore's Populate: stale snapshot files from a previous incarnation must not survive the merge.
		BeforeMerge: func(rec *hypervisor.VMRecord) error {
			return cleanSnapshotFiles(rec.RunDir)
		},
		AfterExtract: fc.restoreAfterExtractCOW,
	})
}

// restoreAfterExtractCOW adapts restoreAfterExtract to the RestoreSpec/DirectRestoreSpec AfterExtract signature using the recorded COW path.
func (fc *Firecracker) restoreAfterExtractCOW(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord) (*types.VM, error) {
	cowPath := hypervisor.DiskPathByRole(rec.StorageConfigs, types.StorageRoleCOW)
	if cowPath == "" {
		return nil, fmt.Errorf("no COW disk recorded for %s", vmID)
	}
	return fc.restoreAfterExtract(ctx, vmID, vmCfg, rec, cowPath)
}

func (fc *Firecracker) killForRestore(ctx context.Context, vmID string, rec *hypervisor.VMRecord) error {
	return fc.KillForRestore(ctx, vmID, rec, func(pid int) error {
		return fc.terminateVMM(ctx, rec, pid)
	}, runtimeFiles)
}

// terminateVMM force-terminates rec's VMM; shared by restore and hibernate.
func (fc *Firecracker) terminateVMM(ctx context.Context, rec *hypervisor.VMRecord, pid int) error {
	return fc.forceTerminate(ctx, hypervisor.SocketPath(rec.RunDir), pid)
}

func (fc *Firecracker) restoreAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord, cowPath string) (_ *types.VM, err error) {
	logger := log.WithFunc("firecracker.Restore")

	snapshotCOW := filepath.Join(rec.RunDir, hypervisor.COWRawFileName)
	if snapshotCOW != cowPath {
		if _, statErr := os.Stat(snapshotCOW); statErr == nil {
			if renameErr := os.Rename(snapshotCOW, cowPath); renameErr != nil {
				return nil, fmt.Errorf("move COW: %w", renameErr)
			}
		}
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)

	pid, launchErr := fc.launchProcess(ctx, rec, sockPath, rec.ResolvedNetnsPath())
	if launchErr != nil {
		return nil, fmt.Errorf("launch FC: %w", launchErr)
	}

	defer func() {
		if err != nil {
			fc.AbortLaunch(ctx, pid, sockPath, rec.RunDir, runtimeFiles)
		}
	}()

	// Same-VM: snapshot's UDS path already matches rec.RunDir; "" stays compatible with FC < v1.16.
	if err = loadSnapshotFC(ctx, sockPath, rec.RunDir, nil, ""); err != nil {
		return nil, fmt.Errorf("snapshot/load: %w", err)
	}

	hc := utils.NewSocketHTTPClient(sockPath)
	if err = resumeVM(ctx, hc); err != nil {
		return nil, fmt.Errorf("resume: %w", err)
	}

	logger.Infof(ctx, "VM %s restored from snapshot", vmID)
	return fc.FinalizeRestore(ctx, vmID, vmCfg, rec, pid)
}
