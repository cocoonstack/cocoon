package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/storage"
	storejson "github.com/cocoonstack/cocoon/storage/json"
	"github.com/cocoonstack/cocoon/types"
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
	conf   *Config
	store  storage.Store[hypervisor.VMIndex]
	locker lock.Locker
}

// New creates a CloudHypervisor backend.
func New(conf *config.Config) (*CloudHypervisor, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := &Config{Config: conf}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	locker := flock.New(cfg.IndexLock())
	store := storejson.New[hypervisor.VMIndex](cfg.IndexFile(), locker)
	return &CloudHypervisor{conf: cfg, store: store, locker: locker}, nil
}

func (ch *CloudHypervisor) Type() string { return typ }

// Inspect returns VM for a single VM by ref (ID, name, or prefix).
func (ch *CloudHypervisor) Inspect(ctx context.Context, ref string) (*types.VM, error) {
	var result *types.VM
	return result, ch.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		result = toVM(idx.VMs[id])
		return nil
	})
}

// List returns VM for all known VMs.
func (ch *CloudHypervisor) List(ctx context.Context) ([]*types.VM, error) {
	var result []*types.VM
	return result, ch.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		for _, rec := range idx.VMs {
			if rec == nil {
				continue
			}
			result = append(result, toVM(rec))
		}
		return nil
	})
}

// Delete removes VMs. Running VMs require force=true (stops them first).
func (ch *CloudHypervisor) Delete(ctx context.Context, refs []string, force bool) ([]string, error) {
	ids, err := ch.resolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	return ch.forEachVM(ctx, ids, "Delete", func(ctx context.Context, id string) error {
		rec, loadErr := ch.loadRecord(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		if err := ch.withRunningVM(ctx, &rec, func(_ int) error {
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
		if err := removeVMDirs(rec.RunDir, rec.LogDir); err != nil {
			return fmt.Errorf("cleanup VM dirs: %w", err)
		}
		return ch.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
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

// resolveRef resolves a single ref (ID, name, or prefix) to an exact VM ID.
func (ch *CloudHypervisor) resolveRef(ctx context.Context, ref string) (string, error) {
	var id string
	return id, ch.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		var err error
		id, err = idx.Resolve(ref)
		return err
	})
}

// resolveRefs batch-resolves refs to exact VM IDs under a single lock.
// Duplicate refs that resolve to the same ID are silently deduplicated.
func (ch *CloudHypervisor) resolveRefs(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	return ids, ch.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		var err error
		ids, err = idx.ResolveMany(refs)
		return err
	})
}
