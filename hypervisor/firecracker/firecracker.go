package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/storage"
	storejson "github.com/cocoonstack/cocoon/storage/json"
	"github.com/cocoonstack/cocoon/types"
)

// compile-time interface checks.
var (
	_ hypervisor.Hypervisor = (*Firecracker)(nil)
	_ hypervisor.Watchable  = (*Firecracker)(nil)
)

const typ = "firecracker"

// Firecracker implements hypervisor.Hypervisor using the Firecracker VMM.
// Only OCI images (direct kernel boot) are supported — no UEFI, no cloudimg, no Windows.
type Firecracker struct {
	conf   *Config
	store  storage.Store[hypervisor.VMIndex]
	locker lock.Locker
}

// New creates a Firecracker backend.
func New(conf *config.Config) (*Firecracker, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := &Config{Config: conf}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	locker := flock.New(cfg.IndexLock())
	store := storejson.New[hypervisor.VMIndex](cfg.IndexFile(), locker)
	return &Firecracker{conf: cfg, store: store, locker: locker}, nil
}

func (fc *Firecracker) Type() string { return typ }

// Inspect returns VM for a single VM by ref (ID, name, or prefix).
func (fc *Firecracker) Inspect(ctx context.Context, ref string) (*types.VM, error) {
	var result *types.VM
	return result, fc.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		result = toVM(idx.VMs[id])
		return nil
	})
}

// List returns VM for all known VMs.
func (fc *Firecracker) List(ctx context.Context) ([]*types.VM, error) {
	var result []*types.VM
	return result, fc.store.With(ctx, func(idx *hypervisor.VMIndex) error {
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
func (fc *Firecracker) Delete(ctx context.Context, refs []string, force bool) ([]string, error) {
	ids, err := fc.resolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	return fc.forEachVM(ctx, ids, "Delete", func(ctx context.Context, id string) error {
		rec, loadErr := fc.loadRecord(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		if err := fc.withRunningVM(ctx, &rec, func(_ int) error {
			if !force {
				return fmt.Errorf("running (force required)")
			}
			return fc.stopOne(ctx, id)
		}); err != nil && !errors.Is(err, hypervisor.ErrNotRunning) {
			return fmt.Errorf("stop before delete: %w", err)
		}
		if err := removeVMDirs(rec.RunDir, rec.LogDir); err != nil {
			return fmt.Errorf("cleanup VM dirs: %w", err)
		}
		return fc.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
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

// --- Stubs: implemented in subsequent commits ---

func (fc *Firecracker) Snapshot(_ context.Context, _ string) (*types.SnapshotConfig, io.ReadCloser, error) {
	return nil, nil, fmt.Errorf("firecracker Snapshot not yet implemented")
}

func (fc *Firecracker) Clone(_ context.Context, _ string, _ *types.VMConfig, _ []*types.NetworkConfig, _ *types.SnapshotConfig, _ io.Reader) (*types.VM, error) {
	return nil, fmt.Errorf("firecracker Clone not yet implemented")
}

func (fc *Firecracker) Restore(_ context.Context, _ string, _ *types.VMConfig, _ io.Reader) (*types.VM, error) {
	return nil, fmt.Errorf("firecracker Restore not yet implemented")
}

func (fc *Firecracker) RegisterGC(_ *gc.Orchestrator) {}
