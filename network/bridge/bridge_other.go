//go:build !linux

package bridge

import (
	"context"
	"fmt"
	"runtime"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/types"
)

// Bridge is a placeholder for non-Linux.
type Bridge struct{}

// New returns an error on non-Linux.
func New(_ *config.Config, _ string) (*Bridge, error) {
	return nil, fmt.Errorf("bridge TAP networking requires Linux (running on %s)", runtime.GOOS)
}

// Type returns the provider identifier.
func (b *Bridge) Type() string { return "bridge" }

// Verify is not supported.
func (b *Bridge) Verify(_ context.Context, _ string) error { return errUnsupported }

// Config is not supported.
func (b *Bridge) Config(_ context.Context, _ string, _ int, _ *types.VMConfig, _ ...*types.NetworkConfig) ([]*types.NetworkConfig, error) {
	return nil, errUnsupported
}

// Delete is not supported.
func (b *Bridge) Delete(_ context.Context, _ []string) ([]string, error) { return nil, errUnsupported }

// Inspect is not supported.
func (b *Bridge) Inspect(_ context.Context, _ string) (*types.Network, error) {
	return nil, errUnsupported
}

// List is not supported.
func (b *Bridge) List(_ context.Context) ([]*types.Network, error) { return nil, errUnsupported }

// RegisterGC is a no-op.
func (b *Bridge) RegisterGC(_ *gc.Orchestrator) {}

// CleanupTAPs is a no-op on non-Linux.
func CleanupTAPs(_ []string) []string { return nil }

var errUnsupported = fmt.Errorf("bridge TAP networking requires Linux (running on %s)", runtime.GOOS)
