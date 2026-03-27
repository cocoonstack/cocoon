package service

import (
	"context"
	"fmt"

	"github.com/projecteru2/cocoon/config"
	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/hypervisor/cloudhypervisor"
	imagebackend "github.com/projecteru2/cocoon/images"
	"github.com/projecteru2/cocoon/images/cloudimg"
	"github.com/projecteru2/cocoon/images/oci"
	"github.com/projecteru2/cocoon/network"
	"github.com/projecteru2/cocoon/network/cni"
	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/snapshot/localfile"
)

// Service provides all Cocoon operations, independent of transport (CLI/gRPC).
type Service struct {
	conf       *config.Config
	hypervisor hypervisor.Hypervisor
	images     []imagebackend.Images
	network    network.Network
	snapshot   snapshot.Snapshot
}

// New creates a Service by initializing all backends from config.
func New(ctx context.Context, conf *config.Config) (*Service, error) {
	ociStore, err := oci.New(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("init oci backend: %w", err)
	}

	cloudimgStore, err := cloudimg.New(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("init cloudimg backend: %w", err)
	}

	hyper, err := cloudhypervisor.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init hypervisor: %w", err)
	}

	netProvider, err := cni.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init network: %w", err)
	}

	snapBackend, err := localfile.New(conf)
	if err != nil {
		return nil, fmt.Errorf("init snapshot: %w", err)
	}

	return &Service{
		conf:       conf,
		hypervisor: hyper,
		images:     []imagebackend.Images{ociStore, cloudimgStore},
		network:    netProvider,
		snapshot:   snapBackend,
	}, nil
}

// NewWithBackends creates a Service with pre-initialized backends.
// Used by tests to inject mock implementations.
func NewWithBackends(
	conf *config.Config,
	hyper hypervisor.Hypervisor,
	images []imagebackend.Images,
	net network.Network,
	snap snapshot.Snapshot,
) *Service {
	return &Service{
		conf:       conf,
		hypervisor: hyper,
		images:     images,
		network:    net,
		snapshot:   snap,
	}
}
