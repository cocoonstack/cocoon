package hypervisor

import (
	"context"

	"github.com/cocoonstack/cocoon/types"
)

// VMNetwork converges a VM's host networking. The command layer supplies it so a
// state transition and its network action share one VM ops lock instead of
// splitting across an unlock window (design §5).
type VMNetwork interface {
	Recover(ctx context.Context, vm *types.VM) error
	Quiesce(ctx context.Context, vm *types.VM) error
	Cleanup(ctx context.Context, vmID string) error
}

// SetNetwork injects the host-networking seam. A nil seam leaves every VM's
// plumbing untouched, which is what a library or test embedding wants.
func (b *Backend) SetNetwork(n VMNetwork) { b.Net = n }

// RecoverNetwork rebuilds and un-quiesces a VM's plumbing before launch; the caller holds the VM ops lock.
func (b *Backend) RecoverNetwork(ctx context.Context, rec *VMRecord) error {
	if b.Net == nil {
		return nil
	}
	return b.Net.Recover(ctx, &rec.VM)
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
