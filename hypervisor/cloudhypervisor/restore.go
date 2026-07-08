package cloudhypervisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (ch *CloudHypervisor) Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, snapshot io.Reader, sourceSnapshotID string) (*types.VM, error) {
	return ch.RestoreSequence(ctx, vmRef, hypervisor.RestoreSpec{
		VMCfg:            vmCfg,
		Snapshot:         snapshot,
		SourceSnapshotID: sourceSnapshotID,
		Preflight:        ch.preflightRestore,
		Kill:             ch.killForRestore,
		AfterExtract: func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord) (*types.VM, error) {
			directBoot := isDirectBoot(rec.BootConfig)
			return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, directBoot)
		},
	})
}

func (ch *CloudHypervisor) preflightRestore(srcDir string, rec *hypervisor.VMRecord) error {
	return hypervisor.PreflightRestore(srcDir, ch.conf.RootDir, ch.conf.Config.RunDir, rec, validateSnapshotIntegrity)
}

func (ch *CloudHypervisor) killForRestore(ctx context.Context, vmID string, rec *hypervisor.VMRecord) error {
	return ch.KillForRestore(ctx, vmID, rec, func(pid int) error {
		return ch.terminateVMM(ctx, rec, utils.NewSocketHTTPClient(hypervisor.SocketPath(rec.RunDir)), pid)
	}, runtimeFiles)
}

// terminateVMM force-terminates rec's VMM over hc; shared by restore and hibernate.
func (ch *CloudHypervisor) terminateVMM(ctx context.Context, rec *hypervisor.VMRecord, hc *http.Client, pid int) error {
	return ch.forceTerminate(ctx, hc, rec.ID, hypervisor.SocketPath(rec.RunDir), pid)
}

func (ch *CloudHypervisor) restoreAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord, directBoot bool) (_ *types.VM, err error) {
	logger := log.WithFunc("cloudhypervisor.Restore")

	chConfigPath := filepath.Join(rec.RunDir, configJSONName)
	// rec may have trailing cidata absent from the snapshot (cloudimg post-first-boot); slice to sidecar length.
	meta, metaErr := hypervisor.LoadAndValidateMeta(rec.RunDir, ch.conf.RootDir, ch.conf.Config.RunDir, hypervisor.ExternalPathSet(rec.StorageConfigs))
	if metaErr != nil {
		return nil, fmt.Errorf("load snapshot meta: %w", metaErr)
	}
	diskCount := len(meta.StorageConfigs)
	if diskCount > len(rec.StorageConfigs) {
		return nil, fmt.Errorf("snapshot has %d disks, VM record has %d", diskCount, len(rec.StorageConfigs))
	}

	if err = patchCHConfig(chConfigPath, &patchOptions{
		storageConfigs: rec.StorageConfigs[:diskCount],
		consoleSock:    hypervisor.ConsoleSockPath(rec.RunDir),
		vsockSock:      hypervisor.VsockSockPath(rec.RunDir),
		directBoot:     directBoot,
		diskQueueSize:  vmCfg.DiskQueueSize,
		noDirectIO:     vmCfg.NoDirectIO,
	}); err != nil {
		return nil, fmt.Errorf("patch config: %w", err)
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)
	args := []string{"--api-socket", sockPath}
	ch.saveCmdline(ctx, rec, args)

	pid, launchErr := ch.launchProcess(ctx, rec, sockPath, args, rec.ResolvedNetnsPath())
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
