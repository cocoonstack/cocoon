package core

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
	imagebackend "github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/network"
	bridgenet "github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/network/cni"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/snapshot/localfile"
)

var hypervisorFactories = []hypervisorFactory{
	{config.HypervisorCloudHypervisor, wireHypervisor(cloudhypervisor.New)},
	{config.HypervisorFirecracker, wireHypervisor(firecracker.New)},
}

type hypervisorCtor func(context.Context, *config.Config) (hypervisor.Hypervisor, error)

type hypervisorFactory struct {
	typ  config.HypervisorType
	ctor hypervisorCtor
}

// networkedHypervisor is the constructor contract wireHypervisor adapts: a backend that accepts the network seam.
type networkedHypervisor interface {
	hypervisor.Hypervisor
	SetNetwork(hypervisor.VMNetwork)
}

func InitImageBackends(ctx context.Context, conf *config.Config) ([]imagebackend.Images, error) {
	ociStore, cloudimgStore, err := InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return nil, err
	}
	return []imagebackend.Images{ociStore, cloudimgStore}, nil
}

func InitImageBackendsForPull(ctx context.Context, conf *config.Config) (*oci.OCI, *cloudimg.CloudImg, error) {
	store, err := MetaStore(conf)
	if err != nil {
		return nil, nil, err
	}
	ociStore, err := oci.New(ctx, conf.RootDir, conf.EffectivePoolSize(), store)
	if err != nil {
		return nil, nil, fmt.Errorf("init oci backend: %w", err)
	}
	cloudimgStore, err := cloudimg.New(ctx, conf.RootDir, conf.EffectivePullConns(), store)
	if err != nil {
		return nil, nil, fmt.Errorf("init cloudimg backend: %w", err)
	}
	recheck := pinnedElsewhere(conf)
	ociStore.SetPinnedElsewhere(recheck)
	cloudimgStore.SetPinnedElsewhere(recheck)
	return ociStore, cloudimgStore, nil
}

func InitHypervisor(ctx context.Context, conf *config.Config) (hypervisor.Hypervisor, error) {
	i := slices.IndexFunc(hypervisorFactories, func(f hypervisorFactory) bool { return f.typ == conf.Hypervisor() })
	if i < 0 {
		return nil, fmt.Errorf("unknown hypervisor type: %s", conf.Hypervisor())
	}
	h, err := hypervisorFactories[i].ctor(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("init hypervisor: %w", err)
	}
	return h, nil
}

func InitAllHypervisors(ctx context.Context, conf *config.Config) ([]hypervisor.Hypervisor, error) {
	factories := hypervisorFactories
	// The sandbox data plane pins itself to Firecracker so list/gc never sweep
	// a co-located desktop's Cloud Hypervisor VMs. Backend selection alone must
	// not change the scope of ordinary multi-backend cocoon installations.
	if conf.PinHypervisor {
		factories = onlyHypervisorFactory(conf.Hypervisor())
	}
	result := make([]hypervisor.Hypervisor, 0, len(factories))
	for _, f := range factories {
		h, err := f.ctor(ctx, conf)
		if err != nil {
			return nil, fmt.Errorf("init hypervisor %s: %w", f.typ, err)
		}
		result = append(result, h)
	}
	return result, nil
}

// onlyHypervisorFactory returns the single factory for typ (empty if unknown).
func onlyHypervisorFactory(typ config.HypervisorType) []hypervisorFactory {
	for _, f := range hypervisorFactories {
		if f.typ == typ {
			return []hypervisorFactory{f}
		}
	}
	return nil
}

func InitNetwork(conf *config.Config) (network.Network, error) {
	store, err := MetaStore(conf)
	if err != nil {
		return nil, err
	}
	p, err := cni.New(conf, store)
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
	store, err := MetaStore(conf)
	if err != nil {
		return nil, err
	}
	opts = append(opts, localfile.WithBlobPinner(func(ctx context.Context, blobIDs map[string]struct{}) (func(), error) {
		return PinEnvelopeBlobs(ctx, conf, blobIDs)
	}))
	s, err := localfile.New(conf, MeteringRecorder(ctx, conf), store, opts...)
	if err != nil {
		return nil, fmt.Errorf("init snapshot backend: %w", err)
	}
	return s, nil
}

// PinEnvelopeBlobs locks envelope-sourced pins (clone/restore --from-dir, snapshot import) on the owning backend; digests with no local blob are skipped.
func PinEnvelopeBlobs(ctx context.Context, conf *config.Config, blobIDs map[string]struct{}) (func(), error) {
	if len(blobIDs) == 0 {
		return func() {}, nil
	}
	ociStore, cloudimgStore, err := InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return nil, err
	}
	ociIDs, cloudimgIDs := map[string]struct{}{}, map[string]struct{}{}
	for hex := range blobIDs {
		switch {
		case ociStore.OwnsBlob(hex):
			ociIDs[hex] = struct{}{}
		case cloudimgStore.OwnsBlob(hex):
			cloudimgIDs[hex] = struct{}{}
		}
	}
	releaseOCI, err := ociStore.PinBlobs(ctx, ociIDs)
	if err != nil {
		return nil, err
	}
	releaseCloudimg, err := cloudimgStore.PinBlobs(ctx, cloudimgIDs)
	if err != nil {
		releaseOCI()
		return nil, err
	}
	return func() { releaseOCI(); releaseCloudimg() }, nil
}

func wireHypervisor[H networkedHypervisor](newFn func(*config.Config, metering.Recorder, meta.Store) (H, error)) hypervisorCtor {
	return func(ctx context.Context, c *config.Config) (hypervisor.Hypervisor, error) {
		store, err := MetaStore(c)
		if err != nil {
			return nil, err
		}
		h, err := newFn(c, MeteringRecorder(ctx, c), store)
		if err != nil {
			return nil, err
		}
		h.SetNetwork(NetworkSeam(c))
		return h, nil
	}
}

// pinnedElsewhere unions VM and snapshot blob pins for image GC's under-lock recheck; backends build lazily, GC-path only.
func pinnedElsewhere(conf *config.Config) func(context.Context) (map[string]struct{}, error) {
	type pinner interface {
		PinnedBlobIDs(context.Context) (map[string]struct{}, error)
	}
	return func(ctx context.Context) (map[string]struct{}, error) {
		hypers, err := InitAllHypervisors(ctx, conf)
		if err != nil {
			return nil, err
		}
		snapBackend, err := InitSnapshot(ctx, conf)
		if err != nil {
			return nil, err
		}
		sources := make([]pinner, 0, len(hypers)+1)
		for _, h := range hypers {
			if p, ok := h.(pinner); ok {
				sources = append(sources, p)
			}
		}
		if p, ok := snapBackend.(pinner); ok {
			sources = append(sources, p)
		}
		pins := map[string]struct{}{}
		for _, s := range sources {
			m, err := s.PinnedBlobIDs(ctx)
			if err != nil {
				return nil, err
			}
			maps.Copy(pins, m)
		}
		return pins, nil
	}
}
