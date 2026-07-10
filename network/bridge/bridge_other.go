//go:build !linux

// Package bridge: non-Linux stubs. All Bridge methods return errUnsupported; CleanupTAPs is a no-op.
package bridge

import (
	"context"
	"fmt"
	"runtime"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

var errUnsupported = fmt.Errorf("bridge TAP networking requires Linux (running on %s)", runtime.GOOS)

type Bridge struct{}

func New(_ *config.Config, _ string) (*Bridge, error) {
	return nil, errUnsupported
}

func (b *Bridge) Type() string { return "bridge" }
func (b *Bridge) Verify(_ context.Context, _ string, _ []*types.NetworkConfig) error {
	return errUnsupported
}

func (b *Bridge) Remove(_ context.Context, _ string, _ ...int) error {
	return errUnsupported
}
func (b *Bridge) RegisterGC(_ *gc.Orchestrator) {}

func (b *Bridge) Prepare(_ context.Context, _ string, _ *types.VMConfig) (string, error) {
	return "", errUnsupported
}

func (b *Bridge) Add(_ context.Context, _ string, _ *types.VMConfig, _ ...network.AddSpec) ([]*types.NetworkConfig, error) {
	return nil, errUnsupported
}

func (b *Bridge) Delete(_ context.Context, _ []string) ([]string, error) { return nil, errUnsupported }

func (b *Bridge) Inspect(_ context.Context, _ string) (*types.Network, error) {
	return nil, errUnsupported
}

func (b *Bridge) List(_ context.Context) ([]*types.Network, error) { return nil, errUnsupported }

func CleanupTAPs(_ []string) []string { return nil }
