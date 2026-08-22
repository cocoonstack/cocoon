package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const exportRestartTimeout = 2 * time.Minute

var _ hypervisor.CloudImageExporter = (*CloudHypervisor)(nil)

// ExportCloudImage flattens a cloudimg VM under its ops lock and restores its original running state before the caller performs the registry upload.
func (ch *CloudHypervisor) ExportCloudImage(ctx context.Context, ref, dest string) (vm *types.VM, retErr error) {
	id, err := ch.ResolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	unlock, err := ch.LockVMOps(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()

	rec, err := ch.LoadRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Config.ImageType != types.ImageTypeCloudImg {
		return nil, fmt.Errorf("vm %s uses image type %q; only cloudimg-backed VMs can be exported as qcow2", ref, rec.Config.ImageType)
	}
	cowPath := hypervisor.DiskPathByRole(rec.StorageConfigs, types.StorageRoleCOW)
	if cowPath == "" {
		return nil, fmt.Errorf("vm %s has no root COW disk", ref)
	}
	vm = ch.ToVM(&rec)

	wasRunning := false
	probeErr := ch.WithRunningVM(ctx, &rec, func(int) error {
		wasRunning = true
		return nil
	})
	switch {
	case probeErr == nil:
	case errors.Is(probeErr, hypervisor.ErrNotRunning):
		// A crashed VMM may leave a Running record behind. Reconcile that stale
		// state, but do not resurrect a VM that was already down before export.
		if hypervisor.NeedsDeadConvergence(&rec) {
			if err := ch.ConvergeDead(ctx, id, rec.TransitionGeneration, time.Now()); err != nil {
				return nil, fmt.Errorf("reconcile stopped VM before export: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("inspect running state: %w", probeErr)
	}
	if wasRunning {
		if err := ch.stopOneLocked(ctx, id); err != nil {
			return nil, fmt.Errorf("stop VM before export: %w", err)
		}
		defer func() {
			restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exportRestartTimeout)
			defer cancel()
			if restartErr := ch.startOneLocked(restartCtx, id); restartErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("restart VM after export: %w", restartErr))
			}
		}()
	}

	if err := utils.RunQemuImg(ctx, "convert", "-p", "-f", "qcow2", "-O", "qcow2", "-c", cowPath, dest); err != nil {
		return vm, fmt.Errorf("flatten root disk: %w", err)
	}
	return vm, nil
}
