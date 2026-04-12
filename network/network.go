package network

import (
	"context"
	"errors"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/types"
)

var (
	ErrNotFound      = errors.New("network not found")
	ErrNotConfigured = errors.New("network provider not configured")
)

// Network defines the interface for a network provider (CNI, etc.).
type Network interface {
	Type() string

	// Verify checks whether the network namespace for a VM exists.
	// Returns nil if the netns is present, an error otherwise.
	Verify(ctx context.Context, vmID string) error
	// Config creates network namespace, bridge, and tap for a VM.
	// When existing configs are provided (recovery after host reboot),
	// the netns and tap devices are recreated using the persisted MAC addresses.
	// NOTE: vmCfg.Network may be mutated to record the resolved conflist name.
	Config(ctx context.Context, vmID string, numNICs int, vmCfg *types.VMConfig, existing ...*types.NetworkConfig) ([]*types.NetworkConfig, error)
	Delete(context.Context, []string) ([]string, error)
	Inspect(context.Context, string) (*types.Network, error)
	List(context.Context) ([]*types.Network, error)

	RegisterGC(*gc.Orchestrator)
}
