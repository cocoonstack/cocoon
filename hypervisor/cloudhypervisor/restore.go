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
func (ch *CloudHypervisor) Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, snapshot io.Reader) (*types.VM, error) {
	vmID, rec, directBoot, cowPath, err := ch.prepareRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	_ = os.Remove(cowPath) // best-effort; extractTar overwrites

	if extractErr := utils.ExtractTar(rec.RunDir, snapshot); extractErr != nil {
		ch.MarkError(ctx, vmID)
		return nil, fmt.Errorf("extract snapshot: %w", extractErr)
	}

	return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, directBoot, cowPath)
}

// prepareRestore handles the common setup for Restore and DirectRestore:
// resolve ref, load record, validate state, kill current CH, cleanup.
func (ch *CloudHypervisor) prepareRestore(ctx context.Context, vmRef string) (string, *hypervisor.VMRecord, bool, string, error) {
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

	sockPath := hypervisor.SocketPath(rec.RunDir)
	killErr := ch.WithRunningVM(ctx, &rec, func(pid int) error {
		return ch.forceTerminate(ctx, utils.NewSocketHTTPClient(sockPath), vmID, sockPath, pid)
	})
	if killErr != nil && !errors.Is(killErr, hypervisor.ErrNotRunning) {
		return "", nil, false, "", fmt.Errorf("stop running VM: %w", killErr)
	}
	hypervisor.CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)

	directBoot := isDirectBoot(rec.BootConfig)
	cowPath := ch.cowPath(vmID, directBoot)
	return vmID, &rec, directBoot, cowPath, nil
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
	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: rec.StorageConfigs,
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
