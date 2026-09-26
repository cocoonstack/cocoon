package core

import (
	"context"
	"fmt"
	"testing"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

func TestRecoverRetriesAnAddThatFinishedAnInterruptedTeardown(t *testing.T) {
	fake := &recoverNetwork{addErrs: []error{fmt.Errorf("teardown interrupted, retry: %w", meta.ErrConflict)}}
	n := &NetProviders{cni: fake, bridge: map[string]network.Network{}}
	vm := &types.VM{ID: "vm1", NetworkConfigs: []*types.NetworkConfig{{TAP: "tap0", MAC: "02:00:00:00:00:01"}}}

	if err := n.Recover(t.Context(), vm); err != nil {
		t.Fatalf("Recover = %v, want the start to converge in one attempt", err)
	}
	if fake.adds != 2 {
		t.Fatalf("Add calls = %d, want a second Add after the one that finished the teardown", fake.adds)
	}
}

type recoverNetwork struct {
	addErrs []error
	adds    int
}

func (f *recoverNetwork) Type() string { return "fake" }

func (f *recoverNetwork) Verify(context.Context, string, []*types.NetworkConfig) error {
	return fmt.Errorf("teardown pending")
}

func (f *recoverNetwork) Prepare(context.Context, string, *types.VMConfig) (string, error) {
	return "", nil
}

func (f *recoverNetwork) Add(context.Context, string, *types.VMConfig, ...network.AddSpec) ([]*types.NetworkConfig, error) {
	f.adds++
	if len(f.addErrs) == 0 {
		return nil, nil
	}
	err := f.addErrs[0]
	f.addErrs = f.addErrs[1:]
	return nil, err
}

func (f *recoverNetwork) Remove(context.Context, string, ...int) error { return nil }

func (f *recoverNetwork) Quiesce(context.Context, string) error { return nil }

func (f *recoverNetwork) Unquiesce(context.Context, string) error { return nil }

func (f *recoverNetwork) Delete(context.Context, string) error { return nil }

func (f *recoverNetwork) RegisterGC(*gc.Orchestrator, network.VMInUse) {}
