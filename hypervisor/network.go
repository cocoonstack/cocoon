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

// quiesceAfterStop runs the quiesce a just-committed stop scheduled, gated on the pre-stop record so an unplumbed VM pays no extra read; a failure keeps the pending flag for a later pass.
func (b *Backend) quiesceAfterStop(ctx context.Context, id string, rec *VMRecord) {
	if !needsQuiesce(rec) {
		return
	}
	if err := b.QuiesceIfPending(ctx, id); err != nil {
		log.WithFunc(b.Typ+".quiesceAfterStop").Warnf(ctx, "%v", err)
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
