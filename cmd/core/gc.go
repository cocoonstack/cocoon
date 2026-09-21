package core

import (
	"context"
	"errors"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock/vmlock"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/snapshot/localfile"
)

// NewGCOrchestrator registers every collector; both hypervisor backends join so a sweep protects blobs pinned by either.
func NewGCOrchestrator(ctx context.Context, conf *config.Config, snapOpts ...localfile.Option) (*gc.Orchestrator, error) {
	backends, err := InitImageBackends(ctx, conf)
	if err != nil {
		return nil, err
	}
	netProvider, err := InitNetwork(conf)
	if err != nil {
		return nil, err
	}
	snapBackend, err := InitSnapshot(ctx, conf, snapOpts...)
	if err != nil {
		return nil, err
	}
	hypers, err := InitAllHypervisors(ctx, conf)
	if err != nil {
		return nil, err
	}

	o := gc.New()
	for _, b := range backends {
		b.RegisterGC(o)
	}
	for _, hyper := range hypers {
		hyper.RegisterGC(o)
	}
	gc.Register(o, hypervisor.CgroupGCModule(conf.CgroupParentDir()))
	inUse := vmInUse(hypers)
	netProvider.RegisterGC(o, inUse)
	gc.Register(o, bridge.GCModule(conf.BridgeTAPPrefix(), inUse))
	gc.Register(o, vmlock.GCModule(conf.RootDir))
	snapBackend.RegisterGC(o)
	return o, nil
}

func vmInUse(hypers []hypervisor.Hypervisor) network.VMInUse {
	return func(ctx context.Context, ref string) (bool, error) {
		for _, h := range hypers {
			_, err := h.Inspect(ctx, ref)
			if err == nil {
				return true, nil
			}
			if !errors.Is(err, hypervisor.ErrNotFound) {
				return false, err
			}
		}
		return false, nil
	}
}
