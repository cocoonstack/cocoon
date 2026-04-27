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

// Restore stages a snapshot tar before replacing the running VM state.
func (fc *Firecracker) Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, snapshot io.Reader) (*types.VM, error) {
	if err := hypervisor.ValidateHostCPU(vmCfg.CPU); err != nil {
		return nil, err
	}
	// FC cannot change machine config after snapshot/load.
	if checkErr := fc.validateRestoreOverrides(ctx, vmRef, vmCfg); checkErr != nil {
		return nil, checkErr
	}

	vmID, rec, cowPath, err := fc.resolveForRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	// Use a clean sibling staging dir before touching the live runDir.
	stagingDir, cleanupStaging, err := hypervisor.PrepareStagingDir(rec.RunDir, snapshot)
	if err != nil {
		return nil, err
	}
	defer cleanupStaging()

	// Pre-flight: validate the staged snapshot fully BEFORE killing the
	// running VM. A malformed snapshot must not cost an outage.
	if err := fc.preflightRestore(stagingDir, rec); err != nil {
		return nil, fmt.Errorf("snapshot preflight: %w", err)
	}

	// Once staging succeeds, stop the current VM and swap files into place.
	if killErr := fc.killForRestore(ctx, vmID, rec); killErr != nil {
		return nil, killErr
	}

	// Lock every writable disk (COW + data disks). withCOWPathLocked alone
	// would leave a stale data-<x>.raw.cocoon-clone-backup unhealed; the next
	// concurrent clone/snapshot would acquire that path's lock,
	// recoverStaleBackup would rename the backup over the just-restored data
	// disk, silently corrupting it.
	var result *types.VM
	if lockErr := withSourceWritableDisksLocked(rec.StorageConfigs, func() error {
		_ = os.Remove(cowPath)
		if mergeErr := hypervisor.MergeDirInto(stagingDir, rec.RunDir); mergeErr != nil {
			fc.MarkError(ctx, vmID)
			return fmt.Errorf("apply staged snapshot: %w", mergeErr)
		}
		var restoreErr error
		result, restoreErr = fc.restoreAfterExtract(ctx, vmID, vmCfg, rec, cowPath)
		return restoreErr
	}); lockErr != nil {
		return nil, lockErr
	}
	return result, nil
}

// resolveForRestore loads the record and validates running state.
func (fc *Firecracker) resolveForRestore(ctx context.Context, vmRef string) (string, *hypervisor.VMRecord, string, error) {
	vmID, rec, err := fc.ResolveForRestore(ctx, vmRef)
	if err != nil {
		return "", nil, "", err
	}
	return vmID, rec, fc.conf.COWRawPath(vmID), nil
}

// killForRestore stops the running FC process and cleans up runtime files.
func (fc *Firecracker) killForRestore(ctx context.Context, vmID string, rec *hypervisor.VMRecord) error {
	return fc.KillForRestore(ctx, vmID, rec, func(pid int) error {
		sockPath := hypervisor.SocketPath(rec.RunDir)
		return fc.forceTerminate(ctx, sockPath, pid)
	}, runtimeFiles)
}

// restoreAfterExtract resumes from snapshot data already placed in runDir.
func (fc *Firecracker) restoreAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord, cowPath string) (_ *types.VM, err error) {
	logger := log.WithFunc("firecracker.Restore")

	defer func() {
		if err != nil {
			fc.MarkError(ctx, vmID)
		}
	}()

	snapshotCOW := filepath.Join(rec.RunDir, cowFileName)
	if snapshotCOW != cowPath {
		if _, statErr := os.Stat(snapshotCOW); statErr == nil {
			if renameErr := os.Rename(snapshotCOW, cowPath); renameErr != nil {
				return nil, fmt.Errorf("move COW: %w", renameErr)
			}
		}
	}

	if vmCfg.Storage > 0 {
		if err = hypervisor.ExpandRawImage(cowPath, vmCfg.Storage); err != nil {
			return nil, fmt.Errorf("resize COW: %w", err)
		}
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)

	withNetwork := len(rec.NetworkConfigs) > 0
	pid, launchErr := fc.launchProcess(ctx, rec, sockPath, withNetwork)
	if launchErr != nil {
		return nil, fmt.Errorf("launch FC: %w", launchErr)
	}

	defer func() {
		if err != nil {
			fc.AbortLaunch(ctx, pid, sockPath, rec.RunDir, runtimeFiles)
		}
	}()

	// Restore keeps the same VM, so drives and TAPs stay unchanged.
	if err = loadSnapshotFC(ctx, sockPath, rec.RunDir, nil); err != nil {
		return nil, fmt.Errorf("snapshot/load: %w", err)
	}

	hc := utils.NewSocketHTTPClient(sockPath)
	if err = resumeVM(ctx, hc); err != nil {
		return nil, fmt.Errorf("resume: %w", err)
	}

	logger.Infof(ctx, "VM %s restored from snapshot", vmID)
	return fc.FinalizeRestore(ctx, vmID, vmCfg, rec, pid)
}

// validateRestoreOverrides rejects CPU or memory changes FC cannot apply after load.
func (fc *Firecracker) validateRestoreOverrides(ctx context.Context, vmRef string, vmCfg *types.VMConfig) error {
	vmID, err := fc.ResolveRef(ctx, vmRef)
	if err != nil {
		return nil // resolve will fail again in prepareRestore with a proper error
	}
	rec, err := fc.LoadRecord(ctx, vmID)
	if err != nil {
		return nil
	}
	if vmCfg.CPU != rec.Config.CPU {
		return fmt.Errorf("--cpu %d not supported: Firecracker cannot change CPU after snapshot/load (VM has %d)", vmCfg.CPU, rec.Config.CPU)
	}
	if vmCfg.Memory != rec.Config.Memory {
		return fmt.Errorf("--memory not supported: Firecracker cannot change memory after snapshot/load")
	}
	return nil
}
