package firecracker

import (
	"context"
	"errors"
	"fmt"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock/flock"
	storejson "github.com/cocoonstack/cocoon/storage/json"
)

// compile-time interface checks.
var (
	_ hypervisor.Hypervisor = (*Firecracker)(nil)
	_ hypervisor.Watchable  = (*Firecracker)(nil)
	_ hypervisor.Direct     = (*Firecracker)(nil)
)

const typ = "firecracker"

// Firecracker implements hypervisor.Hypervisor using the Firecracker VMM.
// Only OCI images (direct kernel boot) are supported — no UEFI, no cloudimg, no Windows.
type Firecracker struct {
	*hypervisor.Backend
	conf *Config
}

// New creates a Firecracker backend.
func New(conf *config.Config) (*Firecracker, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := NewConfig(conf)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	locker := flock.New(cfg.IndexLock())
	store := storejson.New[hypervisor.VMIndex](cfg.IndexFile(), locker)
	return &Firecracker{
		Backend: &hypervisor.Backend{
			Typ:    typ,
			Conf:   cfg,
			DB:     store,
			Locker: locker,
		},
		conf: cfg,
	}, nil
}

// Delete removes VMs. Running VMs require force=true (stops them first).
func (fc *Firecracker) Delete(ctx context.Context, refs []string, force bool) ([]string, error) {
	ids, err := fc.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	return fc.ForEachVM(ctx, ids, "Delete", func(ctx context.Context, id string) error {
		rec, loadErr := fc.LoadRecord(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		if err := fc.WithRunningVM(ctx, &rec, func(_ int) error {
			if !force {
				return fmt.Errorf("running (force required)")
			}
			return fc.stopOne(ctx, id)
		}); err != nil && !errors.Is(err, hypervisor.ErrNotRunning) {
			return fmt.Errorf("stop before delete: %w", err)
		}
		if err := hypervisor.RemoveVMDirs(rec.RunDir, rec.LogDir); err != nil {
			return fmt.Errorf("cleanup VM dirs: %w", err)
		}
		return fc.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
			r := idx.VMs[id]
			if r == nil {
				return hypervisor.ErrNotFound
			}
			delete(idx.Names, r.Config.Name)
			delete(idx.VMs, id)
			return nil
		})
	})
}
