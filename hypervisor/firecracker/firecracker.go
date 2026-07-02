package firecracker

import (
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/metering"
)

const typ = "firecracker"

var (
	_ hypervisor.Hypervisor = (*Firecracker)(nil)
	_ hypervisor.Watchable  = (*Firecracker)(nil)
	_ hypervisor.Direct     = (*Firecracker)(nil)
)

// Firecracker implements hypervisor.Hypervisor using the Firecracker VMM.
// Only OCI images (direct kernel boot) are supported — no UEFI, no cloudimg, no Windows.
type Firecracker struct {
	*hypervisor.Backend
	conf *Config
}

// New creates a Firecracker backend. rec may be nil; the backend falls back to NopRecorder for emit calls.
func New(conf *config.Config, rec metering.Recorder) (*Firecracker, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := NewConfig(conf)
	backend, err := hypervisor.NewBackend(typ, cfg, rec)
	if err != nil {
		return nil, err
	}
	return &Firecracker{Backend: backend, conf: cfg}, nil
}

// Delete removes VMs. Running VMs require force=true (stops them first).
func (fc *Firecracker) Delete(ctx context.Context, refs []string, force bool) ([]string, error) {
	return fc.DeleteAll(ctx, refs, force, fc.stopOne)
}
