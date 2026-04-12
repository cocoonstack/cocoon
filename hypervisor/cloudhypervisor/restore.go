package cloudhypervisor

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

// Restore reverts a running VM to a previous snapshot's state.
//
// The CH process is killed and restarted with the snapshot's memory and disk state.
// Network is preserved — same netns, same tap, same MAC/IP.
// vmCfg carries the final resource config (already validated >= snapshot values).
//
// The incoming snapshot is extracted into a scratch staging directory
// BEFORE the running VM is killed. A truncated or corrupt snapshot
// stream therefore errors out with the previous runnable state still
// intact; only a fully extracted snapshot is swapped into rec.RunDir.
func (ch *CloudHypervisor) Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, snapshot io.Reader) (*types.VM, error) {
	if err := validateHostCPU(vmCfg.CPU); err != nil {
		return nil, err
	}

	vmID, rec, directBoot, cowPath, err := ch.resolveForRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	// Stage: extract into a sibling scratch dir while the VM is still
	// running. If extraction fails, clean up and return — the VM is
	// untouched and the caller can retry.
	//
	// Clear any remnants of a previously interrupted restore: an
	// abrupt exit (SIGKILL, panic, power loss) may have left files
	// behind that the deferred RemoveAll never ran, and ExtractTar
	// only overwrites entries present in the new tar. Without the
	// pre-clear, MergeDirInto would later drag those orphan files
	// into rec.RunDir and poison the restored VM state.
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

	// Commit: kill the running VM, then swap staged files into runDir.
	// killForRestore already marked-error on partial kill failures.
	if killErr := ch.killForRestore(ctx, vmID, rec); killErr != nil {
		return nil, killErr
	}
	// MergeDirInto uses os.Rename which atomically replaces an
	// existing destination, so a pre-remove of cowPath is redundant.
	if mergeErr := hypervisor.MergeDirInto(stagingDir, rec.RunDir); mergeErr != nil {
		ch.MarkError(ctx, vmID)
		return nil, fmt.Errorf("apply staged snapshot: %w", mergeErr)
	}

	return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, directBoot, cowPath)
}

// resolveForRestore loads the VM record and validates the state is
// runnable. Does NOT kill the running VM — that's deferred to
// killForRestore, which runs only after the snapshot has been
// successfully staged. DirectRestore still uses prepareRestore
// (below) which does the old combined flow because it consumes a
// pre-extracted local directory, not a streamable tar.
func (ch *CloudHypervisor) resolveForRestore(ctx context.Context, vmRef string) (string, *hypervisor.VMRecord, bool, string, error) {
	vmID, err := ch.ResolveRef(ctx, vmRef)
	if err != nil {
		return "", nil, false, "", err
	}
	rec, err := ch.LoadRecord(ctx, vmID)
	if err != nil {
		return "", nil, false, "", err
	}
	if rec.State != types.VMStateRunning {
		return "", nil, false, "", fmt.Errorf("vm %s is %s, must be running to restore", vmID, rec.State)
	}
	directBoot := isDirectBoot(rec.BootConfig)
	cowPath := ch.cowPath(vmID, directBoot)
	return vmID, &rec, directBoot, cowPath, nil
}

// killForRestore terminates the running CH process and cleans up
// runtime files. Called after a snapshot has been staged successfully.
// On partial termination failure the VM record is marked with Error
// state so `vm ls` surfaces the broken state rather than leaving a
// stale "running" record.
func (ch *CloudHypervisor) killForRestore(ctx context.Context, vmID string, rec *hypervisor.VMRecord) error {
	sockPath := hypervisor.SocketPath(rec.RunDir)
	killErr := ch.WithRunningVM(ctx, rec, func(pid int) error {
		return ch.forceTerminate(ctx, utils.NewSocketHTTPClient(sockPath), vmID, sockPath, pid)
	})
	if killErr != nil && !errors.Is(killErr, hypervisor.ErrNotRunning) {
		ch.MarkError(ctx, vmID)
		return fmt.Errorf("stop running VM: %w", killErr)
	}
	hypervisor.CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
	return nil
}

// prepareRestore is used by DirectRestore (local-dir-based restore
// that doesn't go through tar streaming) and retains the legacy
// "resolve + kill in one call" flow. Stream Restore uses
// resolveForRestore + killForRestore so extraction runs before
// destructive work.
func (ch *CloudHypervisor) prepareRestore(ctx context.Context, vmRef string) (string, *hypervisor.VMRecord, bool, string, error) {
	vmID, rec, directBoot, cowPath, err := ch.resolveForRestore(ctx, vmRef)
	if err != nil {
		return "", nil, false, "", err
	}
	if killErr := ch.killForRestore(ctx, vmID, rec); killErr != nil {
		return "", nil, false, "", killErr
	}
	return vmID, rec, directBoot, cowPath, nil
}

// restoreAfterExtract contains all restore logic after snapshot data is in runDir.
// Shared by Restore (tar stream) and DirectRestore (direct file copy).
func (ch *CloudHypervisor) restoreAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord, directBoot bool, cowPath string) (_ *types.VM, err error) {
	logger := log.WithFunc("cloudhypervisor.Restore")

	defer func() {
		if err != nil {
			ch.MarkError(ctx, vmID)
		}
	}()

	chConfigPath := filepath.Join(rec.RunDir, "config.json")
	// Use activeDisks to match the filtered view that buildVMConfig
	// produced on the snapshotted start — a post-first-boot cloudimg
	// snapshot has [overlay] only, while rec.StorageConfigs still
	// carries [overlay, cidata]. patchCHConfig's disk-count guard
	// would otherwise hard-fail after prepareRestore has already
	// torn down the running VM.
	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: activeDisks(rec),
		consoleSock:    hypervisor.ConsoleSockPath(rec.RunDir),
		directBoot:     directBoot,
		windows:        vmCfg.Windows,
		cpu:            vmCfg.CPU,
		memory:         vmCfg.Memory,
	}, nil, nil); err != nil {
		return nil, fmt.Errorf("patch config: %w", err)
	}

	if vmCfg.Storage > 0 {
		if err = qemuExpandImage(ctx, cowPath, vmCfg.Storage, directBoot); err != nil {
			return nil, fmt.Errorf("resize COW: %w", err)
		}
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)
	args := []string{"--api-socket", sockPath}
	ch.saveCmdline(ctx, rec, args)

	withNetwork := len(rec.NetworkConfigs) > 0
	pid, launchErr := ch.launchProcess(ctx, rec, sockPath, args, withNetwork)
	if launchErr != nil {
		return nil, fmt.Errorf("launch CH: %w", launchErr)
	}

	defer func() {
		if err != nil {
			ch.AbortLaunch(ctx, pid, sockPath, rec.RunDir, runtimeFiles)
		}
	}()

	if err = restoreVM(ctx, sockPath, rec.RunDir); err != nil {
		return nil, fmt.Errorf("vm.restore: %w", err)
	}
	hc := utils.NewSocketHTTPClient(sockPath)
	if err = resumeVM(ctx, hc); err != nil {
		return nil, fmt.Errorf("vm.resume: %w", err)
	}

	now := time.Now()
	if err = ch.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
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
