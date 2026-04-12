package firecracker

import (
	"context"
	"errors"
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
	stagingDir := rec.RunDir + ".restore-staging"
	if rmErr := os.RemoveAll(stagingDir); rmErr != nil {
		return nil, fmt.Errorf("clear staging dir: %w", rmErr)
	}
	if mkErr := os.MkdirAll(stagingDir, 0o700); mkErr != nil {
		return nil, fmt.Errorf("create staging dir: %w", mkErr)
	}
	defer os.RemoveAll(stagingDir) //nolint:errcheck
	if extractErr := utils.ExtractTar(stagingDir, snapshot); extractErr != nil {
		return nil, fmt.Errorf("extract snapshot: %w", extractErr)
	}

	// Once staging succeeds, stop the current VM and swap files into place.
	if killErr := fc.killForRestore(ctx, vmID, rec); killErr != nil {
		return nil, killErr
	}

	var result *types.VM
	if lockErr := withCOWPathLocked(cowPath, func() error {
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

// resolveForRestore loads the record without stopping the running VM.
func (fc *Firecracker) resolveForRestore(ctx context.Context, vmRef string) (string, *hypervisor.VMRecord, string, error) {
	vmID, err := fc.ResolveRef(ctx, vmRef)
	if err != nil {
		return "", nil, "", err
	}
	rec, err := fc.LoadRecord(ctx, vmID)
	if err != nil {
		return "", nil, "", err
	}
	if rec.State != types.VMStateRunning {
		return "", nil, "", fmt.Errorf("vm %s is %s, must be running to restore", vmID, rec.State)
	}
	cowPath := fc.conf.COWRawPath(vmID)
	return vmID, &rec, cowPath, nil
}

// killForRestore stops the running FC process and cleans up runtime files.
func (fc *Firecracker) killForRestore(ctx context.Context, vmID string, rec *hypervisor.VMRecord) error {
	sockPath := hypervisor.SocketPath(rec.RunDir)
	killErr := fc.WithRunningVM(ctx, rec, func(pid int) error {
		return fc.forceTerminate(ctx, sockPath, pid)
	})
	if killErr != nil && !errors.Is(killErr, hypervisor.ErrNotRunning) {
		fc.MarkError(ctx, vmID)
		return fmt.Errorf("stop running VM: %w", killErr)
	}
	hypervisor.CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
	return nil
}

// prepareRestore is the direct-restore helper that keeps the legacy resolve+kill flow.
func (fc *Firecracker) prepareRestore(ctx context.Context, vmRef string) (string, *hypervisor.VMRecord, string, error) {
	vmID, rec, cowPath, err := fc.resolveForRestore(ctx, vmRef)
	if err != nil {
		return "", nil, "", err
	}
	if killErr := fc.killForRestore(ctx, vmID, rec); killErr != nil {
		return "", nil, "", killErr
	}
	return vmID, rec, cowPath, nil
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
		if err = expandRawImage(cowPath, vmCfg.Storage); err != nil {
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

	now := time.Now()
	if err = fc.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
		r := idx.VMs[vmID]
		if r == nil {
			return fmt.Errorf("vm %s disappeared from index", vmID)
		}
		r.Config = *vmCfg
		r.State = types.VMStateRunning
		r.StartedAt = &now
		r.UpdatedAt = now
		return nil
	}); err != nil {
		return nil, fmt.Errorf("update record: %w", err)
	}

	logger.Infof(ctx, "VM %s restored from snapshot", vmID)

	info := rec.VM
	info.Config = *vmCfg
	info.State = types.VMStateRunning
	info.PID = pid
	info.SocketPath = hypervisor.SocketPath(rec.RunDir)
	info.StartedAt = &now
	info.UpdatedAt = now
	return &info, nil
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
