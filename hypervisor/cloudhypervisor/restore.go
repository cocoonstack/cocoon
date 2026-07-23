package cloudhypervisor

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

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
		Preflight: func(srcDir string, rec *hypervisor.VMRecord) error {
			return ch.preflightRestore(ctx, srcDir, rec, vmCfg)
		},
		Kill: ch.killForRestore,
		// Same sweep as DirectRestore's Populate: stale snapshot files from a previous incarnation must not survive the merge.
		BeforeMerge: func(rec *hypervisor.VMRecord) error {
			return cleanSnapshotFiles(rec.RunDir)
		},
		AfterExtract: func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *hypervisor.VMRecord) (*types.VM, error) {
			directBoot := hypervisor.IsDirectBoot(rec.BootConfig)
			return ch.restoreAfterExtract(ctx, vmID, vmCfg, rec, directBoot)
		},
	})
}

func (ch *CloudHypervisor) preflightRestore(ctx context.Context, srcDir string, rec *hypervisor.VMRecord, vmCfg *types.VMConfig) error {
	chCfg, err := parseCHConfig(filepath.Join(srcDir, configJSONName))
	if err != nil {
		return fmt.Errorf("parse snapshot config: %w", err)
	}
	vmCfg.RestoreMode = resolveRestoreMode(ctx, vmCfg.RestoreMode, chCfg.Memory)
	if err := ch.conf.PreflightRestore(srcDir, rec, func(dir string, sidecar []*types.StorageConfig) error {
		return validateSnapshotIntegrityParsed(dir, sidecar, chCfg)
	}); err != nil {
		return err
	}
	return validateRestoreNICs(chCfg, rec)
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
	meta, metaErr := ch.conf.LoadAndValidateMeta(rec.RunDir)
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
	args := []string{apiSocketFlag, sockPath}
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

	if err = restoreVM(ctx, hc, rec.RunDir, vmCfg.RestoreMode); err != nil {
		return nil, fmt.Errorf("vm.restore: %w", err)
	}
	if err = resumeVM(ctx, hc); err != nil {
		return nil, fmt.Errorf("vm.resume: %w", err)
	}

	logger.Infof(ctx, "VM %s restored from snapshot", vmID)
	return ch.FinalizeRestore(ctx, vmID, vmCfg, rec, pid)
}

// resolveRestoreMode defaults plain private-anon snapshots to mmap — the mapping vm.restore takes keeps the staged file's inode alive past snapshot GC — and downgrades an explicit mmap on hugepages/shared to eager copy with a warning; CH would downgrade silently.
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

// validateRestoreNICs rejects restore when the VM's NIC identity drifted since capture (net resize): vm.restore replays the snapshot's guest MACs verbatim, which would diverge from the live CNI/DB identity.
func validateRestoreNICs(chCfg *chVMConfig, rec *hypervisor.VMRecord) error {
	if len(chCfg.Nets) != len(rec.NetworkConfigs) {
		return fmt.Errorf("snapshot has %d NICs, vm has %d; NIC identity must match for restore", len(chCfg.Nets), len(rec.NetworkConfigs))
	}
	for i, n := range chCfg.Nets {
		if !strings.EqualFold(n.MAC, rec.NetworkConfigs[i].MAC) {
			return fmt.Errorf("nic %d MAC drifted since snapshot (%s -> %s, net resize?); recreate via clone instead", i, n.MAC, rec.NetworkConfigs[i].MAC)
		}
	}
	return nil
}
