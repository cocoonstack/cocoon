package cloudhypervisor

import (
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/metering"
)

const typ = "cloud-hypervisor"

var (
	_ hypervisor.Hypervisor = (*CloudHypervisor)(nil)
	_ hypervisor.Direct     = (*CloudHypervisor)(nil)
)

// CloudHypervisor implements hypervisor.Hypervisor.
type CloudHypervisor struct {
	*hypervisor.Backend
	conf *Config
}

// New creates a CloudHypervisor backend. rec may be nil; the backend falls back to NopRecorder for emit calls.
func New(conf *config.Config, rec metering.Recorder, store meta.Store) (*CloudHypervisor, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := NewConfig(conf)
	backend, err := hypervisor.NewBackend(typ, cfg, rec, store)
	if err != nil {
		return nil, err
	}
	return &CloudHypervisor{Backend: backend, conf: cfg}, nil
}

// Delete removes VMs. Running VMs require force=true (stops them first).
func (ch *CloudHypervisor) Delete(ctx context.Context, refs []string, force bool) ([]string, error) {
	return ch.DeleteAll(ctx, refs, force, ch.stopOneLocked, nil)
}
