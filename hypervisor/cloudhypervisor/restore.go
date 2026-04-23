package cloudhypervisor

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Restore stages a snapshot tar before replacing the running VM state.
func (ch *CloudHypervisor) Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, snapshot io.Reader) (*types.VM, error) {
	if err := hypervisor.ValidateHostCPU(vmCfg.CPU); err != nil {
		return nil, err
	}

	vmID, rec, directBoot, cowPath, err := ch.resolveForRestore(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	// Use a clean sibling staging dir before touching the live runDir.
	stagingDir, cleanupStaging, err := hypervisor.PrepareStagingDir(rec.RunDir, snapshot)
	if err != nil {
		return nil, err
	}
	defer cleanupStaging()

	// Once staging succeeds, stop the current VM and swap files into place.
	if killErr := ch.killForRestore(ctx, vmID, rec); killErr != nil {
		return nil, killErr
	}
	if mergeErr := hypervisor.MergeDirInto(stagingDir, rec.RunDir); mergeErr != nil {
		ch.MarkError(ctx, vmID)
		return nil, fmt.Errorf("apply staged snapshot: %w", mergeErr)
	}

	return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, directBoot, cowPath)
}

// resolveForRestore loads the record and validates running state.
func (ch *CloudHypervisor) resolveForRestore(ctx context.Context, vmRef string) (string, *hypervisor.VMRecord, bool, string, error) {
	vmID, rec, err := ch.ResolveForRestore(ctx, vmRef)
	if err != nil {
		return "", nil, false, "", err
	}
	directBoot := isDirectBoot(rec.BootConfig)
	cowPath := ch.cowPath(vmID, directBoot)
	return vmID, rec, directBoot, cowPath, nil
}

// killForRestore stops the running CH process and cleans up runtime files.
func (ch *CloudHypervisor) killForRestore(ctx context.Context, vmID string, rec *hypervisor.VMRecord) error {
	sockPath := hypervisor.SocketPath(rec.RunDir)
	return ch.KillForRestore(ctx, vmID, rec, func(pid int) error {
		return ch.forceTerminate(ctx, utils.NewSocketHTTPClient(sockPath), vmID, sockPath, pid)
	}, runtimeFiles)
}

// prepareRestore is the direct-restore helper that keeps the legacy resolve+kill flow.
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

// restoreAfterExtract resumes from snapshot data already placed in runDir.
func (ch *CloudHypervisor) restoreAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord, directBoot bool, cowPath string) (_ *types.VM, err error) {
	logger := log.WithFunc("cloudhypervisor.Restore")

	defer func() {
		if err != nil {
			ch.MarkError(ctx, vmID)
		}
	}()

	chConfigPath := filepath.Join(rec.RunDir, "config.json")
	// activeDisks keeps post-first-boot cloudimg restore aligned with config.json.
	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: activeDisks(rec),
		consoleSock:    hypervisor.ConsoleSockPath(rec.RunDir),
		directBoot:     directBoot,
		windows:        vmCfg.Windows,
		cpu:            vmCfg.CPU,
		memory:         vmCfg.Memory,
		diskQueueSize:  vmCfg.DiskQueueSize,
		noDirectIO:     vmCfg.NoDirectIO,
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

	hc := utils.NewSocketHTTPClient(sockPath)

	if err = restoreVM(ctx, hc, rec.RunDir, vmCfg.OnDemand); err != nil {
		return nil, fmt.Errorf("vm.restore: %w", err)
	}
	if err = resumeVM(ctx, hc); err != nil {
		return nil, fmt.Errorf("vm.resume: %w", err)
	}

	logger.Infof(ctx, "VM %s restored from snapshot", vmID)
	return ch.FinalizeRestore(ctx, vmID, vmCfg, rec, pid)
}
