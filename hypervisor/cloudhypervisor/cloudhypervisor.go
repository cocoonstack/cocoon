package cloudhypervisor

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
	_ hypervisor.Hypervisor = (*CloudHypervisor)(nil)
	_ hypervisor.Direct     = (*CloudHypervisor)(nil)
	_ hypervisor.Watchable  = (*CloudHypervisor)(nil)
)

const typ = "cloud-hypervisor"

// CloudHypervisor implements hypervisor.Hypervisor.
type CloudHypervisor struct {
	*hypervisor.Backend
	conf *Config
}

// New creates a CloudHypervisor backend.
func New(conf *config.Config) (*CloudHypervisor, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := NewConfig(conf)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	locker := flock.New(cfg.IndexLock())
	store := storejson.New[hypervisor.VMIndex](cfg.IndexFile(), locker)
	return &CloudHypervisor{
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
func (ch *CloudHypervisor) Delete(ctx context.Context, refs []string, force bool) ([]string, error) {
	ids, err := ch.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	return ch.ForEachVM(ctx, ids, "Delete", func(ctx context.Context, id string) error {
		rec, loadErr := ch.LoadRecord(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		if err := ch.WithRunningVM(ctx, &rec, func(_ int) error {
			if !force {
				return fmt.Errorf("running (force required)")
			}
			return ch.stopOne(ctx, id)
		}); err != nil && !errors.Is(err, hypervisor.ErrNotRunning) {
			return fmt.Errorf("stop before delete: %w", err)
		}
		// Remove dirs BEFORE deleting the DB record so that a dir-cleanup
		// failure keeps the record intact and the user can retry vm rm.
		// This also ensures the ID lands in the succeeded list for network cleanup.
		if err := hypervisor.RemoveVMDirs(rec.RunDir, rec.LogDir); err != nil {
			return fmt.Errorf("cleanup VM dirs: %w", err)
		}
		return ch.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
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
