package core

import (
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
	imagebackend "github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	"github.com/cocoonstack/cocoon/network"
	bridgenet "github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/network/cni"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/snapshot/localfile"
)

var hypervisorFactories = []hypervisorFactory{
	{config.HypervisorCH, func(ctx context.Context, c *config.Config) (hypervisor.Hypervisor, error) {
		return cloudhypervisor.New(c, MeteringRecorder(ctx, c))
	}},
	{config.HypervisorFirecracker, func(ctx context.Context, c *config.Config) (hypervisor.Hypervisor, error) {
		return firecracker.New(c, MeteringRecorder(ctx, c))
	}},
}

type hypervisorFactory struct {
	typ  config.HypervisorType
	ctor func(context.Context, *config.Config) (hypervisor.Hypervisor, error)
}

func InitBackends(ctx context.Context, conf *config.Config) ([]imagebackend.Images, hypervisor.Hypervisor, error) {
	backends, err := InitImageBackends(ctx, conf)
	if err != nil {
		return nil, nil, err
	}
	hyper, err := InitHypervisor(ctx, conf)
	if err != nil {
		return nil, nil, err
	}
	return backends, hyper, nil
}

func InitImageBackends(ctx context.Context, conf *config.Config) ([]imagebackend.Images, error) {
	ociStore, cloudimgStore, err := InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return nil, err
	}
	return []imagebackend.Images{ociStore, cloudimgStore}, nil
}

func InitImageBackendsForPull(ctx context.Context, conf *config.Config) (*oci.OCI, *cloudimg.CloudImg, error) {
	ociStore, err := oci.New(ctx, conf.RootDir, conf.EffectivePoolSize())
	if err != nil {
		return nil, nil, fmt.Errorf("init oci backend: %w", err)
	}
	cloudimgStore, err := cloudimg.New(ctx, conf.RootDir, conf.EffectivePullConns())
	if err != nil {
		return nil, nil, fmt.Errorf("init cloudimg backend: %w", err)
	}
	return ociStore, cloudimgStore, nil
}

func InitHypervisor(ctx context.Context, conf *config.Config) (hypervisor.Hypervisor, error) {
	ctor := findHypervisorFactory(conf.Hypervisor())
	if ctor == nil {
		return nil, fmt.Errorf("unknown hypervisor type: %s", conf.Hypervisor())
	}
	h, err := ctor(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("init hypervisor: %w", err)
	}
	return h, nil
}

func InitAllHypervisors(ctx context.Context, conf *config.Config) ([]hypervisor.Hypervisor, error) {
	result := make([]hypervisor.Hypervisor, 0, len(hypervisorFactories))
	for _, f := range hypervisorFactories {
		h, err := f.ctor(ctx, conf)
		if err != nil {
			return nil, fmt.Errorf("init hypervisor %s: %w", f.typ, err)
		}
		result = append(result, h)
	}
	return result, nil
}

func InitNetwork(conf *config.Config) (network.Network, error) {
	p, err := cni.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init network: %w", err)
	}
	return p, nil
}

func InitBridgeNetwork(conf *config.Config, bridgeDev string) (network.Network, error) {
	p, err := bridgenet.New(conf, bridgeDev)
	if err != nil {
		return nil, fmt.Errorf("init bridge network: %w", err)
	}
	return p, nil
}

func InitSnapshot(ctx context.Context, conf *config.Config, opts ...localfile.Option) (snapshot.Snapshot, error) {
	s, err := localfile.New(conf, MeteringRecorder(ctx, conf), opts...)
	if err != nil {
		return nil, fmt.Errorf("init snapshot backend: %w", err)
	}
	return s, nil
}

func findHypervisorFactory(typ config.HypervisorType) func(context.Context, *config.Config) (hypervisor.Hypervisor, error) {
	for _, f := range hypervisorFactories {
		if f.typ == typ {
			return f.ctor
		}
	}
	return nil
}
