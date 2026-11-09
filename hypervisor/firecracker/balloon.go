package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/cocoonstack/cocoon/extend/balloon"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func (fc *Firecracker) BalloonResize(ctx context.Context, vmRef string, spec balloon.Spec) (balloon.Status, error) {
	hc, rec, unlock, err := fc.LockedRunningOp(ctx, vmRef)
	if err != nil {
		return balloon.Status{}, err
	}
	defer unlock()
	return resizeBalloon(ctx, hc, &rec, spec)
}

func (fc *Firecracker) BalloonStatus(ctx context.Context, vmRef string) (*balloon.Status, error) {
	_, rec, err := fc.ResolveAndLoad(ctx, vmRef)
	if err != nil {
		return nil, err
	}
	hc, _, err := fc.RunningVMClient(ctx, vmRef)
	if err != nil {
		if errors.Is(err, hypervisor.ErrNotRunning) {
			return nil, nil
		}
		return nil, err
	}
	cfg, err := getVMConfig(ctx, hc)
	if err != nil || cfg.Balloon == nil {
		return nil, err
	}
	return new(balloonStatus(&rec, cfg.Balloon.AmountMiB)), nil
}

// resizeBalloon refuses a paused guest instead of resuming it and rounds the target down to MiB, the unit of the Firecracker balloon API.
func resizeBalloon(ctx context.Context, hc *http.Client, rec *hypervisor.VMRecord, spec balloon.Spec) (balloon.Status, error) {
	info, err := getInstanceInfo(ctx, hc)
	if err != nil {
		return balloon.Status{}, err
	}
	if info.State != vmStateRunning {
		return balloon.Status{}, fmt.Errorf("vm %s is %s in the VMM: %w", rec.ID, info.State, hypervisor.ErrNotRunning)
	}
	cfg, err := getVMConfig(ctx, hc)
	if err != nil {
		return balloon.Status{}, err
	}
	if cfg.Balloon == nil {
		return balloon.Status{}, fmt.Errorf("vm %s has no balloon device: %w", rec.ID, balloon.ErrUnsupported)
	}
	target, err := spec.Resolve(int64(cfg.MachineConfig.MemSizeMiB) << 20)
	if err != nil {
		return balloon.Status{}, err
	}
	mib := int(target >> 20)
	if err := patchBalloon(ctx, hc, mib); err != nil {
		return balloon.Status{}, err
	}
	return balloonStatus(rec, mib), nil
}

func balloonStatus(rec *hypervisor.VMRecord, targetMiB int) balloon.Status {
	def, _ := hypervisor.BalloonSize(rec.Config.Config)
	return balloon.Status{Target: int64(targetMiB) << 20, Default: def >> 20 << 20}
}

func resetCloneBalloon(ctx context.Context, hc *http.Client, cfg *types.Config) error {
	def, ok := hypervisor.BalloonSize(*cfg)
	if !ok {
		return nil
	}
	live, err := getVMConfig(ctx, hc)
	mib := int(def >> 20)
	if err != nil || live.Balloon == nil || live.Balloon.AmountMiB == mib {
		return err
	}
	return patchBalloon(ctx, hc, mib)
}
