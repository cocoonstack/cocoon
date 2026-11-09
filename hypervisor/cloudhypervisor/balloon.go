package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/cocoonstack/cocoon/extend/balloon"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func (ch *CloudHypervisor) BalloonResize(ctx context.Context, vmRef string, spec balloon.Spec) (balloon.Status, error) {
	hc, rec, unlock, err := ch.LockedRunningOp(ctx, vmRef)
	if err != nil {
		return balloon.Status{}, err
	}
	defer unlock()
	info, err := getVMInfo(ctx, hc)
	if err != nil {
		return balloon.Status{}, err
	}
	return resizeBalloon(ctx, hc, &rec, info, spec)
}

func (ch *CloudHypervisor) BalloonStatus(ctx context.Context, vmRef string) (*balloon.Status, error) {
	_, rec, err := ch.ResolveAndLoad(ctx, vmRef)
	if err != nil {
		return nil, err
	}
	hc, _, err := ch.RunningVMClient(ctx, vmRef)
	if err != nil {
		if errors.Is(err, hypervisor.ErrNotRunning) {
			return nil, nil
		}
		return nil, err
	}
	info, err := getVMInfo(ctx, hc)
	if err != nil || info.Config.Balloon == nil {
		return nil, err
	}
	st := balloonStatus(&rec, info)
	return &st, nil
}

// resizeBalloon refuses a paused guest instead of resuming it: a migration source or receiver is paused on purpose.
func resizeBalloon(ctx context.Context, hc *http.Client, rec *hypervisor.VMRecord, info *chVMInfoResponse, spec balloon.Spec) (balloon.Status, error) {
	if info.State != chStateRunning {
		return balloon.Status{}, fmt.Errorf("vm %s is %s in the VMM: %w", rec.ID, info.State, hypervisor.ErrNotRunning)
	}
	if info.Config.Balloon == nil {
		return balloon.Status{}, fmt.Errorf("vm %s has no balloon device: %w", rec.ID, balloon.ErrUnsupported)
	}
	target, err := spec.Resolve(info.Config.Memory.Size)
	if err != nil {
		return balloon.Status{}, err
	}
	if err := resizeBalloonVM(ctx, hc, target); err != nil {
		return balloon.Status{}, fmt.Errorf("vm.resize: %w", err)
	}
	st := balloonStatus(rec, info)
	st.Target = target
	return st, nil
}

func balloonStatus(rec *hypervisor.VMRecord, info *chVMInfoResponse) balloon.Status {
	def, _ := hypervisor.BalloonSize(rec.Config.Config)
	return balloon.Status{
		Target:  info.Config.Balloon.Size,
		Actual:  new(info.Config.Memory.Size - info.MemoryActualSize),
		Default: def,
	}
}

func resetCloneBalloon(ctx context.Context, hc *http.Client, snap *chBalloon, cfg *types.Config) error {
	def, ok := hypervisor.BalloonSize(*cfg)
	if snap == nil || !ok || snap.Size == def {
		return nil
	}
	return resizeBalloonVM(ctx, hc, def)
}
