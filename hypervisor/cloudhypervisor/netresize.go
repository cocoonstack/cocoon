package cloudhypervisor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// ejectWaitTimeout bounds the wait for guest B0EJ; Linux acks < 1 s, Windows can take 10–20 s.
const ejectWaitTimeout = 30 * time.Second

// chNICOps drives CH's net devices for the shared resize driver; removal blocks on the guest's ACPI eject.
type chNICOps struct {
	hc *http.Client
}

func (o chNICOps) LiveNICs(ctx context.Context) ([]hypervisor.LiveNIC, error) {
	info, err := getVMInfo(ctx, o.hc)
	if err != nil {
		return nil, err
	}
	live := make([]hypervisor.LiveNIC, 0, len(info.Config.Nets))
	for _, n := range info.Config.Nets {
		idx, ok := network.TAPIndex(n.TAP)
		if !ok {
			idx = -1
		}
		live = append(live, hypervisor.LiveNIC{ID: n.ID, Index: idx})
	}
	return live, nil
}

func (o chNICOps) AddNIC(ctx context.Context, _ int, nc *types.NetworkConfig) (string, error) {
	return addCocoonNIC(ctx, o.hc, nc)
}

func (o chNICOps) RemoveNIC(ctx context.Context, id string) error {
	if err := removeDeviceVM(ctx, o.hc, id); err != nil {
		return err
	}
	if err := waitDeviceEjected(ctx, o.hc, id); err != nil {
		return fmt.Errorf("%w: %w", hypervisor.ErrEjectPending, err)
	}
	return nil
}

func (chNICOps) TAPQueues(cpu int) int { return network.NetNumQueues(cpu) }

func (ch *CloudHypervisor) NetResize(ctx context.Context, vmRef string, spec netresize.Spec, plumbing netresize.Plumbing) (netresize.Result, error) {
	if err := spec.Normalize(); err != nil {
		return netresize.Result{}, err
	}
	hc, rec, info, unlock, err := ch.lockedDeviceOp(ctx, vmRef)
	if err != nil {
		return netresize.Result{}, err
	}
	defer unlock()
	if _, err = convergeOrphanedPause(ctx, hc, rec.ID, info); err != nil {
		return netresize.Result{}, err
	}
	return ch.NetResizeWith(ctx, rec.ID, &rec, chNICOps{hc: hc}, plumbing, spec.Target)
}
