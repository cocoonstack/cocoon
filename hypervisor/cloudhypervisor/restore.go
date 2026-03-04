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

	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

// Restore reverts a running VM to a previous snapshot's state.
//
// The CH process is killed and restarted with the snapshot's memory and disk state.
// Network is preserved — same netns, same tap, same MAC/IP.
// vmCfg carries the final resource config (already validated >= snapshot values).
func (ch *CloudHypervisor) Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, snapshot io.Reader) (*types.VM, error) {
	logger := log.WithFunc("cloudhypervisor.Restore")

	vmID, err := ch.resolveRef(ctx, vmRef)
	if err != nil {
		return nil, err
	}

	rec, err := ch.loadRecord(ctx, vmID)
	if err != nil {
		return nil, err
	}

	if rec.State != types.VMStateRunning {
		return nil, fmt.Errorf("VM %s is %s, must be running to restore", vmID, rec.State)
	}

	// Phase 1: kill current CH process.
	sockPath := socketPath(rec.RunDir)
	killErr := ch.withRunningVM(&rec, func(pid int) error {
		return ch.forceTerminate(ctx, utils.NewSocketHTTPClient(sockPath), vmID, sockPath, pid)
	})
	if killErr != nil && !errors.Is(killErr, hypervisor.ErrNotRunning) {
		return nil, fmt.Errorf("stop running VM: %w", killErr)
	}
	cleanupRuntimeFiles(ctx, rec.RunDir)

	// Phase 2: replace state files with snapshot data.
	directBoot := isDirectBoot(rec.BootConfig)
	cowPath := ch.cowPath(vmID, directBoot)
	_ = os.Remove(cowPath) // best-effort; extractTar overwrites

	if extractErr := utils.ExtractTar(rec.RunDir, snapshot); extractErr != nil {
		ch.markError(ctx, vmID)
		return nil, fmt.Errorf("extract snapshot: %w", extractErr)
	}

	// Phase 3: patch config.json with current resource values.
	// Storage/network configs are passed through unchanged (same VM, same paths)
	// so patchCHConfig's count validation passes. Only CPU/memory/balloon change.
	chConfigPath := filepath.Join(rec.RunDir, "config.json")
	if patchErr := patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: rec.StorageConfigs,
		networkConfigs: rec.NetworkConfigs,
		consoleSock:    consoleSockPath(rec.RunDir),
		directBoot:     directBoot,
		cpu:            vmCfg.CPU,
		memory:         vmCfg.Memory,
		vmName:         vmCfg.Name,
		dnsServers:     ch.conf.DNSServers(),
	}); patchErr != nil {
		ch.markError(ctx, vmID)
		return nil, fmt.Errorf("patch config: %w", patchErr)
	}

	// Phase 4: resize COW if current storage > snapshot's.
	if vmCfg.Storage > 0 {
		if resizeErr := resizeCOW(ctx, cowPath, vmCfg.Storage, directBoot); resizeErr != nil {
			ch.markError(ctx, vmID)
			return nil, fmt.Errorf("resize COW: %w", resizeErr)
		}
	}

	// Phase 5: launch new CH → vm.restore → vm.resume.
	args := []string{"--api-socket", sockPath}
	ch.saveCmdline(ctx, &rec, args)

	withNetwork := len(rec.NetworkConfigs) > 0
	pid, launchErr := ch.launchProcess(ctx, &rec, sockPath, args, withNetwork)
	if launchErr != nil {
		ch.markError(ctx, vmID)
		return nil, fmt.Errorf("launch CH: %w", launchErr)
	}

	hc := utils.NewSocketHTTPClient(sockPath)
	if restoreErr := restoreVM(ctx, hc, rec.RunDir); restoreErr != nil {
		ch.abortLaunch(ctx, pid, sockPath, rec.RunDir)
		ch.markError(ctx, vmID)
		return nil, fmt.Errorf("vm.restore: %w", restoreErr)
	}
	if resumeErr := resumeVM(ctx, hc); resumeErr != nil {
		ch.abortLaunch(ctx, pid, sockPath, rec.RunDir)
		ch.markError(ctx, vmID)
		return nil, fmt.Errorf("vm.resume: %w", resumeErr)
	}

	// Phase 6: update DB — keep state=running, update timestamps and config.
	now := time.Now()
	if updateErr := ch.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
		r := idx.VMs[vmID]
		if r == nil {
			return fmt.Errorf("VM %s disappeared from index", vmID)
		}
		r.Config = *vmCfg
		r.State = types.VMStateRunning
		r.StartedAt = &now
		r.UpdatedAt = now
		return nil
	}); updateErr != nil {
		ch.abortLaunch(ctx, pid, sockPath, rec.RunDir)
		return nil, fmt.Errorf("update record: %w", updateErr)
	}

	logger.Infof(ctx, "VM %s restored from snapshot", vmID)

	// Build return value reflecting the persisted state.
	// rec was loaded before the DB update, so construct a fresh VM
	// with the updated fields (Config, timestamps) to avoid returning stale data.
	info := rec.VM
	info.Config = *vmCfg
	info.State = types.VMStateRunning
	info.PID = pid
	info.SocketPath = socketPath(rec.RunDir)
	info.StartedAt = &now
	info.UpdatedAt = now
	return &info, nil
}
