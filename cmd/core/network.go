package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	bridgenet "github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/types"
)

var (
	netOnce sync.Once
	netSeam *NetProviders
)

var _ hypervisor.VMNetwork = (*NetProviders)(nil)

// NetProviders resolves and caches the network provider each VM's record names.
type NetProviders struct {
	conf *config.Config

	mu     sync.Mutex
	cni    network.Network
	bridge map[string]network.Network
}

// NetworkSeam returns the process-wide seam, so both backends share one provider cache.
func NetworkSeam(conf *config.Config) *NetProviders {
	netOnce.Do(func() { netSeam = newNetProviders(conf) })
	return netSeam
}

func newNetProviders(conf *config.Config) *NetProviders {
	return &NetProviders{conf: conf, bridge: map[string]network.Network{}}
}

// ForVM picks the provider from the VM's persisted network state.
func (n *NetProviders) ForVM(vm *types.VM) (network.Network, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if vm.ResolvedNetBackend() != types.BackendBridge {
		return n.cniLocked()
	}
	dev := vm.ResolvedNetBridgeDev()
	if dev == "" {
		return nil, fmt.Errorf("bridge backend but no bridge device persisted")
	}
	if cached, ok := n.bridge[dev]; ok {
		return cached, nil
	}
	p, err := InitBridgeNetwork(n.conf, dev)
	if err != nil {
		return nil, err
	}
	n.bridge[dev] = p
	return p, nil
}

// Recover verifies, rebuilds or un-quiesces the VM's plumbing; its error aborts the launch, since half-built networking strands the guest.
func (n *NetProviders) Recover(ctx context.Context, vm *types.VM) error {
	backend := vm.ResolvedNetBackend()
	if backend == "" || (backend == types.BackendBridge && len(vm.NetworkConfigs) == 0) {
		return nil
	}
	p, err := n.ForVM(vm)
	if err != nil {
		return err
	}
	if p.Verify(ctx, vm.ID, vm.NetworkConfigs) == nil {
		return p.Unquiesce(ctx, vm.ID)
	}
	log.WithFunc("core.NetProviders.Recover").Warnf(ctx, "network missing for VM %s, recovering", vm.ID)
	if _, prepErr := p.Prepare(ctx, vm.ID, &vm.Config); prepErr != nil {
		return fmt.Errorf("prepare netns: %w", prepErr)
	}
	if len(vm.NetworkConfigs) == 0 {
		return nil
	}
	_, err = p.Add(ctx, vm.ID, &vm.Config, network.AddRecover(vm.NetworkConfigs)...)
	return err
}

// Quiesce brings a stopped VM's host NICs down.
func (n *NetProviders) Quiesce(ctx context.Context, vm *types.VM) error {
	p, err := n.ForVM(vm)
	if err != nil {
		return err
	}
	return p.Quiesce(ctx, vm.ID)
}

// Cleanup releases a VM's networking inside the delete protocol; a partial CNI
// failure leaves the tombstone for retry or GC to resume.
func (n *NetProviders) Cleanup(ctx context.Context, vmID string) error {
	bridgenet.CleanupTAPs([]string{vmID})
	p, err := n.cniOnly()
	if err != nil {
		// Lazy CNI; OK to skip for bridge-only setups.
		return nil
	}
	_, err = p.Delete(ctx, []string{vmID})
	return err
}

// cniOnly resolves the CNI provider without a VM record, for the ID-only teardown path.
func (n *NetProviders) cniOnly() (network.Network, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cniLocked()
}

func (n *NetProviders) cniLocked() (network.Network, error) {
	if n.cni == nil {
		p, err := InitNetwork(n.conf)
		if err != nil {
			return nil, err
		}
		n.cni = p
	}
	return n.cni, nil
}
