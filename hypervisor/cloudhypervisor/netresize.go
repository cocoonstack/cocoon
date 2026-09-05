package cloudhypervisor

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/hypervisor"
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
		if strings.HasPrefix(n.ID, cocoonNetIDPrefix) {
			live = append(live, hypervisor.LiveNIC{ID: n.ID, MAC: n.MAC, TAP: n.TAP})
		}
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
	return waitDeviceEjected(ctx, o.hc, id)
}

func (ch *CloudHypervisor) NetResize(ctx context.Context, vmRef string, spec netresize.Spec, plumbing netresize.Plumbing) (netresize.Result, error) {
	if err := spec.Normalize(); err != nil {
		return netresize.Result{}, err
	}
	hc, vmID, err := ch.RunningVMClient(ctx, vmRef)
	if err != nil {
		return netresize.Result{}, err
	}
	unlock, err := ch.LockVMOps(ctx, vmID)
	if err != nil {
		return netresize.Result{}, err
	}
	defer unlock()
	// Entrypoint discipline (design §5): a resize must not plumb NICs onto a VM whose delete was interrupted. Reload under the lock: a resize that won the lock first may have changed the NIC set after this one loaded.
	rec, err := ch.EntryGuardLoad(ctx, vmID)
	if err != nil {
		return netresize.Result{}, err
	}
	info, err := getVMInfo(ctx, hc)
	if err != nil {
		return netresize.Result{}, err
	}
	if _, err = convergeOrphanedPause(ctx, hc, vmID, info); err != nil {
		return netresize.Result{}, err
	}
	return ch.NetResizeWith(ctx, vmID, &rec, chNICOps{hc: hc}, plumbing, spec.Target)
}
