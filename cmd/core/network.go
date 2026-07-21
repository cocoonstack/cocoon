package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// NetProviders resolves and caches the network provider each VM's record names;
// one instance is shared across concurrent VM operations.
type NetProviders struct {
	conf *config.Config

	mu     sync.Mutex
	cni    network.Network
	bridge map[string]network.Network
}

func NewNetProviders(conf *config.Config) *NetProviders {
	return &NetProviders{conf: conf, bridge: map[string]network.Network{}}
}

// ForVM picks the provider from the VM's persisted network state.
func (n *NetProviders) ForVM(vm *types.VM) (network.Network, error) {
	if vm == nil {
		return nil, fmt.Errorf("no VM record")
	}
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

// Lifecycle is the locked network convergence the hypervisor runs on start and stop.
func (n *NetProviders) Lifecycle() hypervisor.NetLifecycle {
	return hypervisor.NetLifecycle{Recover: n.recoverNetwork, Quiesce: n.quiesceNetwork}
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

// recoverNetwork verifies the VM's plumbing, rebuilding it when missing and lifting the
// stop-time quiesce otherwise. Its error aborts the launch: booting a VM whose
// host networking is half-built strands it with no way to reach the network.
func (n *NetProviders) recoverNetwork(ctx context.Context, vm *types.VM) error {
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
	log.WithFunc("core.NetProviders.recoverNetwork").Warnf(ctx, "network missing for VM %s, recovering", vm.ID)
	if _, prepErr := p.Prepare(ctx, vm.ID, &vm.Config); prepErr != nil {
		return fmt.Errorf("prepare netns: %w", prepErr)
	}
	if len(vm.NetworkConfigs) == 0 {
		return nil
	}
	_, err = p.Add(ctx, vm.ID, &vm.Config, network.AddRecover(vm.NetworkConfigs)...)
	return err
}

func (n *NetProviders) quiesceNetwork(ctx context.Context, vm *types.VM) error {
	p, err := n.ForVM(vm)
	if err != nil {
		return err
	}
	return p.Quiesce(ctx, vm.ID)
}
