package cloudhypervisor

import (
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/extend/fs"
	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/extend/vfio"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/metering"
)

const typ = "cloud-hypervisor"

var (
	_ hypervisor.Hypervisor = (*CloudHypervisor)(nil)
	_ hypervisor.Direct     = (*CloudHypervisor)(nil)
	_ disk.Attacher         = (*CloudHypervisor)(nil)
	_ disk.Lister           = (*CloudHypervisor)(nil)
	_ fs.Attacher           = (*CloudHypervisor)(nil)
	_ fs.Lister             = (*CloudHypervisor)(nil)
	_ vfio.Attacher         = (*CloudHypervisor)(nil)
	_ vfio.Lister           = (*CloudHypervisor)(nil)
	_ netresize.Resizer     = (*CloudHypervisor)(nil)
)

type CloudHypervisor struct {
	*hypervisor.Backend
	conf *Config
}

// New creates a CloudHypervisor backend; a nil rec falls back to NopRecorder.
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

func (ch *CloudHypervisor) Delete(ctx context.Context, refs []string, force bool) ([]string, error) {
	return ch.DeleteAll(ctx, refs, force, ch.stopOneLocked)
}
