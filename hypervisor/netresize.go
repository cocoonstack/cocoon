package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// nicRollbackTimeout bounds a rollback that outlives a canceled resize (guest eject on CH can take 10–20 s).
const nicRollbackTimeout = 60 * time.Second

// ErrEjectPending marks a RemoveNIC whose request the VMM accepted but the guest has not released; the host slot is reclaimed anyway.
var ErrEjectPending = errors.New("guest has not released the device")

// LiveNIC is one NIC device the VMM exposes right now; Index is its host slot (record position), -1 when the VMM reports a device cocoon cannot place.
type LiveNIC struct {
	ID    string
	Index int
}

// NICDeviceOps is the VMM half of a NIC resize; the driver owns host plumbing and record persistence.
type NICDeviceOps interface {
	LiveNICs(ctx context.Context) ([]LiveNIC, error)
	AddNIC(ctx context.Context, index int, nc *types.NetworkConfig) (string, error)
	// RemoveNIC returns once the guest released the device where the VMM waits for that, ErrEjectPending when it never did.
	RemoveNIC(ctx context.Context, id string) error
	// TAPQueues is the queue count a fresh TAP needs so the VMM can open it (CH per vCPU, FC single-queue).
	TAPQueues(cpu int) int
}

// NetResizeWith grows or shrinks the VM's NIC set to target; the caller holds the ops lock and passes the record loaded under it.
func (b *Backend) NetResizeWith(ctx context.Context, vmID string, rec *VMRecord, dev NICDeviceOps, plumbing netresize.Plumbing, target int) (netresize.Result, error) {
	current := len(rec.NetworkConfigs)
	res := netresize.Result{Before: current, After: current}
	live, err := dev.LiveNICs(ctx)
	if err != nil {
		return res, err
	}
	// Reconcile before comparing counts: an interrupted resize leaves a ghost device/TAP that target==current would otherwise never heal.
	if err := reconcileOrphanNICs(ctx, dev, vmID, live, current, plumbing); err != nil {
		return res, err
	}
	switch {
	case target == current:
		return res, nil
	case target > current:
		return b.netResizeAdd(ctx, vmID, rec, dev, plumbing, target, res)
	default:
		return b.netResizeRemove(ctx, vmID, rec, live, dev, plumbing, target, res)
	}
}

func (b *Backend) netResizeAdd(ctx context.Context, vmID string, rec *VMRecord, dev NICDeviceOps, plumbing netresize.Plumbing, target int, res netresize.Result) (netresize.Result, error) {
	logger := log.WithFunc("hypervisor.netResizeAdd")
	from := len(rec.NetworkConfigs)
	res.Added = make([]netresize.NIC, 0, target-from)
	for i := from; i < target; i++ {
		ncs, err := plumbing.Add(ctx, vmID, &rec.Config, network.AddSpec{Index: i, Queues: dev.TAPQueues(rec.Config.CPU)})
		if err != nil {
			return res, fmt.Errorf("nic %d host plumbing: %w", i, err)
		}
		if len(ncs) != 1 || ncs[0] == nil {
			return res, fmt.Errorf("nic %d: plumbing returned %d configs", i, len(ncs))
		}
		nc := ncs[0]
		devID, err := dev.AddNIC(ctx, i, nc)
		if err != nil {
			// Survives Ctrl-C: an abandoned half-add is exactly the divergence the reconcile above exists for.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nicRollbackTimeout)
			if rmErr := plumbing.Remove(rctx, vmID, i); rmErr != nil {
				logger.Warnf(rctx, "rollback host plumbing for nic %d: %v", i, rmErr)
			}
			cancel()
			return res, fmt.Errorf("add nic %d: %w", i, err)
		}
		if err := b.appendNetworkConfig(ctx, vmID, nc); err != nil {
			committed, verifyErr := b.resolveFailedPersist(ctx, dev, plumbing, vmID, nc, devID, i)
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

// resolveFailedPersist tears down a failed NIC persist only on a conclusive lockless re-read miss: fsync can fail after the rename landed, and removing a committed NIC strands record-without-device beyond any retry.
func (b *Backend) resolveFailedPersist(ctx context.Context, dev NICDeviceOps, plumbing netresize.Plumbing, vmID string, nc *types.NetworkConfig, devID string, i int) (bool, error) {
	rec, err := b.PeekRecord(ctx, vmID)
	if err != nil {
		return false, err
	}
	if nicPersisted(rec, nc.TAP) {
		return true, nil
	}
	logger := log.WithFunc("hypervisor.resolveFailedPersist")
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nicRollbackTimeout)
	defer cancel()
	if rmErr := dev.RemoveNIC(rctx, devID); rmErr != nil {
		logger.Warnf(rctx, "rollback device %s after persist failure: %v", devID, rmErr)
	}
	if rmErr := plumbing.Remove(rctx, vmID, i); rmErr != nil {
		logger.Warnf(rctx, "rollback host plumbing for nic %d: %v", i, rmErr)
	}
	return false, nil
}

func (b *Backend) netResizeRemove(ctx context.Context, vmID string, rec *VMRecord, live []LiveNIC, dev NICDeviceOps, plumbing netresize.Plumbing, target int, res netresize.Result) (netresize.Result, error) {
	logger := log.WithFunc("hypervisor.netResizeRemove")
	current := len(rec.NetworkConfigs)
	res.Removed = make([]netresize.NIC, 0, current-target)
	for i := current - 1; i >= target; i-- {
		nc := rec.NetworkConfigs[i]
		if nc == nil {
			return res, fmt.Errorf("nic %d: nil network config", i)
		}
		devID := ""
		if j := slices.IndexFunc(live, func(n LiveNIC) bool { return n.Index == i }); j >= 0 {
			devID = live[j].ID
		}
		if devID == "" {
			// Already gone after an interrupted prior resize: resume with the host/DB halves so retries converge instead of wedging on "no live device".
			logger.Warnf(ctx, "nic %d (%s): no live device, resuming interrupted remove", i, nc.TAP)
		} else if err := dev.RemoveNIC(ctx, devID); err != nil {
			return res, fmt.Errorf("remove nic %d (%s): %w", i, devID, err)
		}
		plumbingErr := plumbing.Remove(ctx, vmID, i)
		if err := b.truncateNetworkConfigs(ctx, vmID, i); err != nil {
			if plumbingErr != nil {
				logger.Warnf(ctx, "host plumbing leak for vm %s nic %d (%s): %v", vmID, i, devID, plumbingErr)
			}
			logger.Errorf(ctx, err, "persistence diverged from the VMM for vm %s nic %d (%s): live device removed, cocoon record retained", vmID, i, devID)
			return res, fmt.Errorf("persist remove nic %d: %w", i, err)
		}
		if plumbingErr != nil {
			msg := fmt.Sprintf("nic %d (%s) host plumbing leaked, cocoon vm rm or gc will reclaim: %v", i, devID, plumbingErr)
			logger.Warn(ctx, msg)
			res.Warnings = append(res.Warnings, msg)
		}
		res.Removed = append(res.Removed, netresize.NIC{Index: i, TAP: nc.TAP, MAC: nc.MAC})
		res.After = i
	}
	return res, nil
}

func (b *Backend) appendNetworkConfig(ctx context.Context, vmID string, nc *types.NetworkConfig) error {
	return b.UpdateRecord(ctx, vmID, func(r *VMRecord) error {
		r.NetworkConfigs = append(r.NetworkConfigs, nc)
		return nil
	})
}

func (b *Backend) truncateNetworkConfigs(ctx context.Context, vmID string, length int) error {
	return b.UpdateRecord(ctx, vmID, func(r *VMRecord) error {
		if length < len(r.NetworkConfigs) {
			r.NetworkConfigs = r.NetworkConfigs[:length]
		}
		return nil
	})
}

// reconcileOrphanNICs removes live NICs in slots the record does not know — leftovers of a resize interrupted between the device add and the DB write — and best-effort reclaims their host slot (bridge TAPs have no DB record, and a leftover TAP wedges every retry at CreateTAP).
func reconcileOrphanNICs(ctx context.Context, dev NICDeviceOps, vmID string, live []LiveNIC, known int, plumbing netresize.Plumbing) error {
	logger := log.WithFunc("hypervisor.reconcileOrphanNICs")
	for _, n := range live {
		if n.Index >= 0 && n.Index < known {
			continue
		}
		removeErr := dev.RemoveNIC(ctx, n.ID)
		if removeErr != nil && !errors.Is(removeErr, ErrEjectPending) {
			return fmt.Errorf("remove orphan NIC %s: %w", n.ID, removeErr)
		}
		// Reclaim the host slot even when the guest never released the device: a late release drops it from the live set, so no later reconcile would see this TAP and retries wedge at CreateTAP.
		if n.Index >= 0 {
			if rmErr := plumbing.Remove(ctx, vmID, n.Index); rmErr != nil {
				logger.Warnf(ctx, "reclaim host slot %d for orphan NIC %s: %v", n.Index, n.ID, rmErr)
			}
		}
		if removeErr != nil {
			return fmt.Errorf("remove orphan NIC %s: %w", n.ID, removeErr)
		}
	}
	return nil
}

func nicPersisted(rec *VMRecord, tap string) bool {
	return rec != nil && slices.ContainsFunc(rec.NetworkConfigs, func(nc *types.NetworkConfig) bool {
		return nc != nil && nc.TAP == tap
	})
}
