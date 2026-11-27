package hypervisor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// ResizeSpec is the size a stopped VM takes at its next cold boot; a zero field keeps the current value.
type ResizeSpec struct {
	CPU     int
	Memory  int64
	Storage int64
}

func (b *Backend) Resize(ctx context.Context, ref string, spec ResizeSpec) (*types.VM, error) {
	id, err := b.resolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	unlock, err := b.LockVMOps(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	rec, err := b.entryGuardLoad(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = b.requireStopped(ctx, &rec); err != nil {
		return nil, err
	}
	old := rec.Config
	cfg := &rec.Config
	cfg.CPU, cfg.Memory, cfg.Storage = cmp.Or(spec.CPU, old.CPU), cmp.Or(spec.Memory, old.Memory), cmp.Or(spec.Storage, old.Storage)
	if err = validateResize(&old, cfg); err != nil {
		return nil, err
	}
	if cfg.CPU == 1 && slices.ContainsFunc(rec.NetworkConfigs, func(nc *types.NetworkConfig) bool {
		return nc != nil && nc.NumQueues > network.NetNumQueues(1)
	}) {
		return nil, fmt.Errorf("vm %s has a multi-queue NIC, which needs 2 vCPUs or more", id)
	}
	if cfg.Storage > old.Storage {
		if err = growCOW(ctx, DiskPathByRole(rec.StorageConfigs, types.StorageRoleCOW), cfg.Storage); err != nil {
			return nil, fmt.Errorf("grow the disk of vm %s: %w", id, err)
		}
	}
	from, to, now := shapeFromConfig(old), shapeFromConfig(*cfg), timeNow()
	if from == to {
		return b.ToVM(&rec), nil
	}
	if err = b.updateVM(ctx, id, func(r *VMRecord) []metering.Entry {
		r.Config.CPU, r.Config.Memory, r.Config.Storage = cfg.CPU, cfg.Memory, cfg.Storage
		return []metering.Entry{
			b.makeEntry(metering.KindVMStorageStop, id, metering.ReasonResize, from, now),
			b.makeEntry(metering.KindVMStorageStart, id, metering.ReasonResize, to, now),
		}
	}); err != nil {
		return nil, err
	}
	return b.ToVM(&rec), nil
}

// requireStopped refuses a VM a record or a live VMM still claims as up; vm stop settles both.
func (b *Backend) requireStopped(ctx context.Context, rec *VMRecord) error {
	if rec.State == types.VMStateCreating {
		return fmt.Errorf("vm %s is still being created", rec.ID)
	}
	switch err := b.WithRunningVM(ctx, rec, func(int) error { return nil }); {
	case err == nil, NeedsDeadConvergence(rec):
		return fmt.Errorf("vm %s is not stopped; run vm stop first", rec.ID)
	case !errors.Is(err, ErrNotRunning):
		return err
	}
	return nil
}

func validateResize(old, cfg *types.VMConfig) error {
	if cfg.Storage < old.Storage {
		return fmt.Errorf("--storage %d is below the current %d; a disk only grows", cfg.Storage, old.Storage)
	}
	if err := cfg.ValidateSize(); err != nil {
		return err
	}
	if err := validateHostCPU(cfg.CPU); err != nil {
		return err
	}
	return cgroup.ResolveKnobs(&cfg.Config).Validate()
}
