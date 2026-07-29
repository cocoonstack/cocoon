package cloudhypervisor

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// ejectWaitTimeout bounds the wait for guest B0EJ; Linux acks < 1 s, Windows can take 10–20 s.
const ejectWaitTimeout = 30 * time.Second

// NetResize brings the VM's NIC count to spec.Target on a running CH VM.
func (ch *CloudHypervisor) NetResize(ctx context.Context, vmRef string, spec netresize.Spec, plumbing netresize.Plumbing) (netresize.Result, error) {
	if err := spec.Normalize(); err != nil {
		return netresize.Result{}, err
	}
	hc, vmID, rec, err := ch.runningVMClientWithRecord(ctx, vmRef)
	if err != nil {
		return netresize.Result{}, err
	}
	unlock, err := ch.LockVMOps(ctx, vmID)
	if err != nil {
		return netresize.Result{}, err
	}
	defer unlock()
	// Entrypoint discipline (design §5): a resize must not plumb NICs onto a
	// VM whose delete was interrupted. Reload under the lock: a resize that
	// won the lock first may have changed the NIC set after this one loaded.
	if rec, err = ch.EntryGuardLoad(ctx, vmID); err != nil {
		return netresize.Result{}, err
	}
	info, err := getVMInfo(ctx, hc)
	if err != nil {
		return netresize.Result{}, err
	}
	if info, err = convergeOrphanedPause(ctx, hc, info); err != nil {
		return netresize.Result{}, err
	}
	current := len(rec.NetworkConfigs)
	res := netresize.Result{Before: current, After: current}
	// Reconcile before comparing counts: an interrupted resize leaves a ghost device/TAP that target==current would otherwise never heal.
	if err = reconcileOrphanNICs(ctx, hc, info, vmID, rec.NetworkConfigs, plumbing); err != nil {
		return res, err
	}
	switch {
	case spec.Target == current:
		return res, nil
	case spec.Target > current:
		return ch.netResizeAdd(ctx, hc, vmID, &rec, plumbing, current, spec.Target, res)
	default:
		return ch.netResizeRemove(ctx, hc, info, vmID, rec.NetworkConfigs, plumbing, current, spec.Target, res)
	}
}

func (ch *CloudHypervisor) netResizeAdd(ctx context.Context, hc *http.Client, vmID string, rec *hypervisor.VMRecord, plumbing netresize.Plumbing, from, target int, res netresize.Result) (netresize.Result, error) {
	logger := log.WithFunc("cloudhypervisor.netResizeAdd")
	res.Added = make([]netresize.NIC, 0, target-from)
	for i := from; i < target; i++ {
		ncs, err := plumbing.Add(ctx, vmID, &rec.Config, network.AddSpec{Index: i})
		if err != nil {
			return res, fmt.Errorf("nic %d host plumbing: %w", i, err)
		}
		if len(ncs) != 1 || ncs[0] == nil {
			return res, fmt.Errorf("nic %d: plumbing returned %d configs", i, len(ncs))
		}
		nc := ncs[0]
		chID, err := addCocoonNIC(ctx, hc, nc)
		if err != nil {
			// Survives Ctrl-C: an abandoned half-add is exactly the divergence the reconcile above exists for.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*ejectWaitTimeout)
			if rmErr := plumbing.Remove(rctx, vmID, i); rmErr != nil {
				logger.Warnf(rctx, "rollback host plumbing for nic %d: %v", i, rmErr)
			}
			cancel()
			return res, fmt.Errorf("vm.add-net nic %d: %w", i, err)
		}
		if err := ch.appendNetworkConfig(ctx, vmID, nc); err != nil {
			committed, verifyErr := ch.resolveFailedPersist(ctx, hc, plumbing, vmID, nc, chID, i)
			if verifyErr != nil {
				return res, fmt.Errorf("persist nic %d: %w; commit state inconclusive: %v (device kept, rerun vm net to reconcile)", i, err, verifyErr)
			}
			if !committed {
				return res, fmt.Errorf("persist nic %d: %w", i, err)
			}
			logger.Warnf(ctx, "persist nic %d reported %v but committed; keeping device", i, err)
		}
		res.Added = append(res.Added, netresize.NIC{Index: i, TAP: nc.TAP, MAC: nc.MAC})
		res.After = i + 1
	}
	return res, nil
}

// resolveFailedPersist re-reads the record after a failed NIC persist (fsync
// can fail after the rename landed) and tears down only on a conclusive miss:
// removing a committed NIC would strand record-without-device, unhealable by a
// same-target retry. Lockless read so a GC index lock can't fake a miss.
func (ch *CloudHypervisor) resolveFailedPersist(ctx context.Context, hc *http.Client, plumbing netresize.Plumbing, vmID string, nc *types.NetworkConfig, chID string, i int) (bool, error) {
	rec, err := ch.PeekRecord(ctx, vmID)
	if err != nil {
		return false, err
	}
	if nicPersisted(rec, nc.MAC) {
		return true, nil
	}
	logger := log.WithFunc("cloudhypervisor.resolveFailedPersist")
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*ejectWaitTimeout)
	defer cancel()
	// Wait for B0EJ before host teardown (symmetric with netResizeRemove).
	if rmErr := removeDeviceVM(rctx, hc, chID); rmErr != nil {
		logger.Warnf(rctx, "rollback vm.remove-device %s after persist failure: %v", chID, rmErr)
	} else if wErr := waitDeviceEjected(rctx, hc, chID); wErr != nil {
		logger.Warnf(rctx, "rollback wait eject %s after persist failure: %v", chID, wErr)
	}
	if rmErr := plumbing.Remove(rctx, vmID, i); rmErr != nil {
		logger.Warnf(rctx, "rollback host plumbing for nic %d: %v", i, rmErr)
	}
	return false, nil
}

func (ch *CloudHypervisor) netResizeRemove(ctx context.Context, hc *http.Client, info *chVMInfoResponse, vmID string, ncs []*types.NetworkConfig, plumbing netresize.Plumbing, current, target int, res netresize.Result) (netresize.Result, error) {
	logger := log.WithFunc("cloudhypervisor.netResizeRemove")
	res.Removed = make([]netresize.NIC, 0, current-target)
	macToID := make(map[string]string, len(info.Config.Nets))
	for _, n := range info.Config.Nets {
		macToID[strings.ToLower(n.MAC)] = n.ID
	}
	for i := current - 1; i >= target; i-- {
		nc := ncs[i]
		if nc == nil {
			return res, fmt.Errorf("nic %d: nil network config", i)
		}
		chID := macToID[strings.ToLower(nc.MAC)]
		if chID == "" {
			// Already ejected by an interrupted prior resize: resume with the host/DB halves so retries converge instead of wedging on "no live device".
			logger.Warnf(ctx, "nic %d MAC %s: no live device, resuming interrupted remove", i, nc.MAC)
		} else {
			if err := removeDeviceVM(ctx, hc, chID); err != nil {
				return res, fmt.Errorf("vm.remove-device nic %d (%s): %w", i, chID, err)
			}
			if err := waitDeviceEjected(ctx, hc, chID); err != nil {
				return res, fmt.Errorf("wait eject nic %d (%s): %w", i, chID, err)
			}
		}
		plumbingErr := plumbing.Remove(ctx, vmID, i)
		if err := ch.truncateNetworkConfigs(ctx, vmID, i); err != nil {
			if plumbingErr != nil {
				logger.Warnf(ctx, "host plumbing leak for vm %s nic %d (%s): %v", vmID, i, chID, plumbingErr)
			}
			logger.Errorf(ctx, err, "persistence diverged from CH for vm %s nic %d (%s): live device removed, cocoon record retained", vmID, i, chID)
			return res, fmt.Errorf("persist remove nic %d: %w", i, err)
		}
		if plumbingErr != nil {
			msg := fmt.Sprintf("nic %d (%s) host plumbing leaked, cocoon vm rm or gc will reclaim: %v", i, chID, plumbingErr)
			logger.Warn(ctx, msg)
			res.Warnings = append(res.Warnings, msg)
		}
		res.Removed = append(res.Removed, netresize.NIC{Index: i, TAP: nc.TAP, MAC: nc.MAC})
		res.After = i
	}
	return res, nil
}

func (ch *CloudHypervisor) appendNetworkConfig(ctx context.Context, vmID string, nc *types.NetworkConfig) error {
	return ch.UpdateRecord(ctx, vmID, func(r *hypervisor.VMRecord) error {
		r.NetworkConfigs = append(r.NetworkConfigs, nc)
		return nil
	})
}

func (ch *CloudHypervisor) truncateNetworkConfigs(ctx context.Context, vmID string, length int) error {
	return ch.UpdateRecord(ctx, vmID, func(r *hypervisor.VMRecord) error {
		if length < len(r.NetworkConfigs) {
			r.NetworkConfigs = r.NetworkConfigs[:length]
		}
		return nil
	})
}

func nicPersisted(rec *hypervisor.VMRecord, mac string) bool {
	return rec != nil && slices.ContainsFunc(rec.NetworkConfigs, func(nc *types.NetworkConfig) bool {
		return nc != nil && strings.EqualFold(nc.MAC, mac)
	})
}

// reconcileOrphanNICs removes cocoon-managed CH devices whose MAC the record does not know — leftovers of a resize interrupted between vm.add-net and the DB write — and best-effort reclaims their host slot (bridge TAPs have no DB record, and a leftover TAP wedges every retry at CreateTAP).
func reconcileOrphanNICs(ctx context.Context, hc *http.Client, info *chVMInfoResponse, vmID string, ncs []*types.NetworkConfig, plumbing netresize.Plumbing) error {
	logger := log.WithFunc("cloudhypervisor.reconcileOrphanNICs")
	known := make(map[string]struct{}, len(ncs))
	for _, nc := range ncs {
		if nc != nil {
			known[strings.ToLower(nc.MAC)] = struct{}{}
		}
	}
	for _, n := range info.Config.Nets {
		if !strings.HasPrefix(n.ID, cocoonNetIDPrefix) {
			continue
		}
		if _, ok := known[strings.ToLower(n.MAC)]; ok {
			continue
		}
		if err := removeDeviceVM(ctx, hc, n.ID); err != nil {
			return fmt.Errorf("eject orphan NIC %s: %w", n.ID, err)
		}
		// Reclaim the host slot even when the eject wait times out (same
		// pattern as resolveFailedPersist's rollback): once the guest finishes
		// a late eject the device vanishes from vm.info, so no later reconcile
		// would ever see this TAP again and it would wedge retries at CreateTAP.
		ejectErr := waitDeviceEjected(ctx, hc, n.ID)
		if idx, ok := network.TAPIndex(n.TAP); ok {
			if rmErr := plumbing.Remove(ctx, vmID, idx); rmErr != nil {
				logger.Warnf(ctx, "reclaim host slot for orphan NIC %s (tap %s): %v", n.ID, n.TAP, rmErr)
			}
		}
		if ejectErr != nil {
			return fmt.Errorf("wait orphan eject %s: %w", n.ID, ejectErr)
		}
	}
	return nil
}
