package network

import (
	"context"
	"errors"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/types"
)

var ErrNotConfigured = errors.New("network provider not configured")

// AddSpec is one NIC's add request; Existing != nil reuses MAC/IP for recovery.
type AddSpec struct {
	Index    int
	Existing *types.NetworkConfig
}

// Network is the per-VM host-side networking provider (CNI, bridge, ...).
type Network interface {
	Type() string
	Verify(ctx context.Context, vmID string) error
	// Prepare provisions per-VM state; returns the netns path or "".
	Prepare(ctx context.Context, vmID string, vmCfg *types.VMConfig) (string, error)
	Add(ctx context.Context, vmID string, vmCfg *types.VMConfig, specs ...AddSpec) ([]*types.NetworkConfig, error)
	Remove(ctx context.Context, vmID string, indices ...int) error
	Delete(context.Context, []string) ([]string, error)
	Inspect(context.Context, string) (*types.Network, error)
	List(context.Context) ([]*types.Network, error)
	RegisterGC(*gc.Orchestrator)
}

// AddRange builds AddSpecs for a contiguous block of fresh NIC indices.
func AddRange(from, count int) []AddSpec {
	out := make([]AddSpec, count)
	for i := range out {
		out[i] = AddSpec{Index: from + i}
	}
	return out
}

// AddRecover builds AddSpecs for re-creating existing NICs (post-reboot recovery).
func AddRecover(existing []*types.NetworkConfig) []AddSpec {
	out := make([]AddSpec, len(existing))
	for i, e := range existing {
		out[i] = AddSpec{Index: i, Existing: e}
	}
	return out
}
