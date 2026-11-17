package hypervisor

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/types"
)

// VMNetwork converges a VM's host networking under the caller's VM ops lock.
type VMNetwork interface {
	Recover(ctx context.Context, vm *types.VM) error
	Quiesce(ctx context.Context, vm *types.VM) error
	Cleanup(ctx context.Context, vmID string) error
}

func (b *Backend) SetNetwork(n VMNetwork) { b.Net = n }

// recoverNetwork rebuilds and un-quiesces a VM's plumbing before launch; the caller holds the VM ops lock.
func (b *Backend) recoverNetwork(ctx context.Context, rec *VMRecord) error {
	if b.Net == nil {
		return nil
	}
	return b.Net.Recover(ctx, &rec.VM)
}

// Under the caller's ops lock the committed generation is rec's, plus one exactly when the stop just landed its transition.
func (b *Backend) quiesceAfterStop(ctx context.Context, id string, rec *VMRecord, wentDown, transitioned bool) error {
	gen, pending := rec.TransitionGeneration, rec.QuiescePending || (wentDown && needsQuiesce(rec))
	if transitioned {
		gen++
	}
	if !pending {
		return nil
	}
	if err := b.quiesceNetwork(ctx, &rec.VM); err != nil {
		return fmt.Errorf("quiesce network for VM %s (pending kept): %w", id, err)
	}
	if err := b.clearQuiescePending(ctx, id, gen); err != nil {
		log.WithFunc(b.Typ+".quiesceAfterStop").Warnf(ctx, "clear pending quiesce for VM %s: %v", id, err)
	}
	return nil
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
