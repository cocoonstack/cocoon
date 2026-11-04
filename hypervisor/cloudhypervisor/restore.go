package cloudhypervisor

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (ch *CloudHypervisor) Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, stream io.Reader, sourceSnapshotID string) (*types.VM, error) {
	// Preflight's meta feeds AfterExtract; cocoon.json is merged verbatim.
	var meta *snapshot.SnapshotMeta
	return ch.RestoreSequence(ctx, vmRef, hypervisor.RestoreSpec{
		VMCfg:            vmCfg,
		Snapshot:         stream,
		SourceSnapshotID: sourceSnapshotID,
		Preflight: func(srcDir string, rec *hypervisor.VMRecord) error {
			var err error
			meta, err = ch.preflightRestore(ctx, srcDir, rec, vmCfg)
			return err
		},
		Kill:  ch.killForRestore,
		Clean: cleanSnapshotFiles,
		AfterExtract: func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord) (*types.VM, error) {
			return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, meta)
		},
	})
}

func (ch *CloudHypervisor) preflightRestore(ctx context.Context, srcDir string, rec *hypervisor.VMRecord, vmCfg *types.VMConfig) (*snapshot.SnapshotMeta, error) {
	chCfg, err := parseCHConfig(filepath.Join(srcDir, configJSONName))
	if err != nil {
		return nil, fmt.Errorf("parse snapshot config: %w", err)
	}
	vmCfg.RestoreMode = resolveRestoreMode(ctx, vmCfg.RestoreMode, chCfg.Memory)
	meta, err := ch.conf.PreflightRestore(srcDir, rec, func(dir string, sidecar []*types.StorageConfig) error {
		return validateSnapshotIntegrity(dir, sidecar, chCfg)
	})
	if err != nil {
		return nil, err
	}
	return meta, validateRestoreNICs(chCfg, rec)
}

func (ch *CloudHypervisor) killForRestore(ctx context.Context, vmID string, rec *hypervisor.VMRecord) error {
	return ch.KillForRestore(ctx, vmID, rec, func(pid int) error {
		return ch.terminateVMM(ctx, rec, utils.NewSocketHTTPClient(hypervisor.SocketPath(rec.RunDir)), pid)
	}, runtimeFiles)
}

func (ch *CloudHypervisor) terminateVMM(ctx context.Context, rec *hypervisor.VMRecord, hc *http.Client, pid int) error {
	return ch.forceTerminate(ctx, hc, rec.ID, hypervisor.SocketPath(rec.RunDir), pid)
}

func (ch *CloudHypervisor) restoreAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord, meta *snapshot.SnapshotMeta) (_ *types.VM, err error) {
	logger := log.WithFunc("cloudhypervisor.restoreAfterExtract")
	directBoot := hypervisor.IsDirectBoot(rec.BootConfig)

	chConfigPath := filepath.Join(rec.RunDir, configJSONName)
	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: hypervisor.RecordDisksForSidecar(meta.StorageConfigs, rec.StorageConfigs),
		consoleSock:    hypervisor.ConsoleSockPath(rec.RunDir),
		vsockSock:      hypervisor.VsockSockPath(rec.RunDir),
		directBoot:     directBoot,
		diskQueueSize:  vmCfg.DiskQueueSize,
		noDirectIO:     vmCfg.NoDirectIO,
		cpu:            vmCfg.CPU,
		queueCPUs:      hypervisor.QueueCPUs(rec),
	}); err != nil {
		return nil, fmt.Errorf("patch config: %w", err)
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)
	args := []string{apiSocketFlag, sockPath}

	// Launch under the target config: a --force cross-config restore changes the vCPU count the scope derives from.
	rec.Config = *vmCfg
	pid, launchErr := ch.launchProcess(ctx, rec, args, rec.ResolvedNetnsPath(), true)
	if launchErr != nil {
		return nil, fmt.Errorf("launch CH: %w", launchErr)
	}

	defer func() {
		if err != nil {
			ch.AbortLaunch(ctx, pid, sockPath, rec.RunDir, runtimeFiles)
		}
	}()

	hc := utils.NewSocketHTTPClient(sockPath)

	if err = restoreVM(ctx, hc, rec.RunDir, vmCfg.RestoreMode); err != nil {
		return nil, fmt.Errorf("vm.restore: %w", err)
	}
	if err = ch.ArmCPUQuota(vmID, &vmCfg.Config); err != nil {
		return nil, err
	}
	if err = resumeVM(ctx, hc); err != nil {
		return nil, fmt.Errorf("vm.resume: %w", err)
	}
	saveConsolePTY(ctx, vmID, rec.RunDir, sockPath, directBoot)

	logger.Infof(ctx, "VM %s restored from snapshot", vmID)
	return ch.FinalizeRestore(ctx, vmID, vmCfg, rec, pid)
}

// mmap keeps the staged snapshot's inode alive past GC; CH would downgrade it silently.
func resolveRestoreMode(ctx context.Context, mode string, mem chMemory) string {
	if !mem.HugePages && !mem.Shared {
		return cmp.Or(mode, restoreModeMmap)
	}
	logger := log.WithFunc("cloudhypervisor.resolveRestoreMode")
	switch mode {
	case restoreModeMmap:
		logger.Warnf(ctx, "restore-mode mmap needs plain private-anon snapshot memory (hugepages=%t, shared=%t): falling back to eager copy; rebuild the golden without hugepages/shared for the fast path", mem.HugePages, mem.Shared)
		return restoreModeCopy
	case "", restoreModeCopy:
		logger.Debugf(ctx, "snapshot memory (hugepages=%t, shared=%t) rules out the mmap fast path; memory loads via eager copy", mem.HugePages, mem.Shared)
	}
	return mode
}

// vm.restore replays the snapshot's guest MACs verbatim, so drifted NIC identity must fail first.
func validateRestoreNICs(chCfg *chVMConfig, rec *hypervisor.VMRecord) error {
	macs := make([]string, len(chCfg.Nets))
	for i, n := range chCfg.Nets {
		macs[i] = n.MAC
	}
	return hypervisor.ValidateRestoreMACs(macs, rec)
}
