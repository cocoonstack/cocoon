package hypervisor

import (
	"context"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/types"
)

// VMNetwork converges a VM's host networking; the command layer supplies it so a state transition and its network action share one VM ops lock (design §5).
type VMNetwork interface {
	Recover(ctx context.Context, vm *types.VM) error
	Quiesce(ctx context.Context, vm *types.VM) error
	Cleanup(ctx context.Context, vmID string) error
}

// SetNetwork injects the host-networking seam; a nil seam leaves every VM's plumbing untouched, as a library or test embedding wants.
func (b *Backend) SetNetwork(n VMNetwork) { b.Net = n }

// RecoverNetwork rebuilds and un-quiesces a VM's plumbing before launch; the caller holds the VM ops lock.
func (b *Backend) RecoverNetwork(ctx context.Context, rec *VMRecord) error {
	if b.Net == nil {
		return nil
	}
	return b.Net.Recover(ctx, &rec.VM)
}

// Under the caller's ops lock the committed generation is rec's, plus one exactly when the stop just landed its transition.
func (b *Backend) quiesceAfterStop(ctx context.Context, id string, rec *VMRecord, transitioned bool) {
	gen, pending := rec.TransitionGeneration, rec.QuiescePending
	if transitioned {
		gen, pending = gen+1, needsQuiesce(rec)
	}
	if !pending {
		return
	}
	logger := log.WithFunc(b.Typ + ".quiesceAfterStop")
	if err := b.quiesceNetwork(ctx, &rec.VM); err != nil {
		logger.Warnf(ctx, "quiesce network for VM %s (pending kept): %v", id, err)
		return
	}
	if err := b.clearQuiescePending(ctx, id, gen); err != nil {
		logger.Warnf(ctx, "clear pending quiesce for VM %s: %v", id, err)
	}
}

func (b *Backend) quiesceNetwork(ctx context.Context, vm *types.VM) error {
	if b.Net == nil {
		return nil
	}
	return b.Net.Quiesce(ctx, vm)
}

func (b *Backend) cleanupNetwork(ctx context.Context, vmID string) error {
	if b.Net == nil {
		return nil
	}
	return b.Net.Cleanup(ctx, vmID)
}
