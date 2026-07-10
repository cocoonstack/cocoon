package hypervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// ReserveVM inserts a "creating" placeholder under id, failing on id/name collision. Re-reserving the placeholder this same create claimed via PrereserveVM adopts it (refreshing blob pins and dirs).
func (b *Backend) ReserveVM(ctx context.Context, id string, vmCfg *types.VMConfig, blobIDs map[string]struct{}, runDir, logDir string) error {
	now := time.Now()
	return b.DB.Update(ctx, func(idx *VMIndex) error {
		if existing := idx.VMs[id]; existing != nil {
			if existing.State == types.VMStateCreating && existing.Config.Name == vmCfg.Name {
				existing.ImageBlobIDs = blobIDs
				existing.RunDir = runDir
				existing.LogDir = logDir
				existing.UpdatedAt = now
				return nil
			}
			return fmt.Errorf("id collision %q (retry)", id)
		}
		if dup, ok := idx.Names[vmCfg.Name]; ok && dup != id {
			return fmt.Errorf("vm name %q already exists (id: %s)", vmCfg.Name, dup)
		}
		idx.VMs[id] = &VMRecord{
			VM: types.VM{
				ID: id, Hypervisor: b.Typ, State: types.VMStateCreating,
				Config: *vmCfg, CreatedAt: now, UpdatedAt: now,
			},
			ImageBlobIDs: blobIDs,
			RunDir:       runDir,
			LogDir:       logDir,
		}
		idx.Names[vmCfg.Name] = id
		return nil
	})
}

// PrereserveVM claims id before host resources (network) are provisioned, so GC always sees an owner for them; CreateSequence/CloneSetup later adopts the placeholder.
func (b *Backend) PrereserveVM(ctx context.Context, id string, vmCfg *types.VMConfig) error {
	return b.ReserveVM(ctx, id, vmCfg, nil, b.Conf.VMRunDir(id), b.Conf.VMLogDir(id))
}

// RollbackCreate removes the placeholder record and its name mapping after a failed create.
func (b *Backend) RollbackCreate(ctx context.Context, id, name string) {
	if err := b.DB.Update(ctx, func(idx *VMIndex) error {
		delete(idx.VMs, id)
		if name != "" && idx.Names[name] == id {
			delete(idx.Names, name)
		}
		return nil
	}); err != nil {
		log.WithFunc(b.Typ+".RollbackCreate").Errorf(ctx, err, "rollback VM %s (name=%s)", id, name)
	}
}

// FinalizeCreate persists the populated VM record (replacing the placeholder) and emits metering vm.storage.start.
func (b *Backend) FinalizeCreate(ctx context.Context, id string, info *types.VM, bootCfg *types.BootConfig, blobIDs map[string]struct{}) error {
	if err := b.DB.Update(ctx, func(idx *VMIndex) error {
		existing, err := idx.GetRecord(id)
		if err != nil {
			return err
		}
		idx.VMs[id] = &VMRecord{
			VM:           *info,
			BootConfig:   bootCfg,
			ImageBlobIDs: blobIDs,
			RunDir:       existing.RunDir,
			LogDir:       existing.LogDir,
		}
		return nil
	}); err != nil {
		return err
	}
	b.Metering.Emit(ctx, b.makeEntry(metering.KindVMStorageStart, id, metering.ReasonBoot, shapeFromConfig(info.Config), time.Now()))
	return nil
}

// CreateSequence is the shared placeholder→finalize create skeleton.
func (b *Backend) CreateSequence(ctx context.Context, id string, spec CreateSpec) (_ *types.VM, err error) {
	blobIDs := ExtractBlobIDs(spec.StorageConfigs, spec.BootConfig)
	_, _, now, cleanup, err := b.reservePlaceholder(ctx, id, spec.VMCfg, blobIDs)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	var bootCopy *types.BootConfig
	if spec.BootConfig != nil {
		bc := *spec.BootConfig
		bootCopy = &bc
	}

	preparedStorage, err := spec.Prepare(ctx, id, spec.VMCfg, spec.StorageConfigs, spec.Net, bootCopy)
	if err != nil {
		return nil, err
	}
	if err = types.ValidateStorageConfigs(preparedStorage); err != nil {
		return nil, fmt.Errorf("storage invariants violated: %w", err)
	}

	info := &types.VM{
		ID: id, Hypervisor: b.Typ, State: types.VMStateCreated,
		Config: *spec.VMCfg, StorageConfigs: preparedStorage,
		NetSetup:  spec.Net,
		CreatedAt: now, UpdatedAt: now,
	}
	if err = b.FinalizeCreate(ctx, id, info, bootCopy, blobIDs); err != nil {
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}
	return info, nil
}

// reservePlaceholder validates host CPU, reserves a "creating" VM record, and ensures its run/log dirs exist; shared by CreateSequence and CloneSetup. On failure it rolls back internally (if needed) and returns a nil cleanup; on success cleanup removes the dirs and rolls back the reservation — the caller decides when to run it.
func (b *Backend) reservePlaceholder(ctx context.Context, id string, vmCfg *types.VMConfig, blobIDs map[string]struct{}) (runDir, logDir string, now time.Time, cleanup func(), err error) {
	if err = ValidateHostCPU(vmCfg.CPU); err != nil {
		return "", "", time.Time{}, nil, err
	}
	now = time.Now()
	runDir = b.Conf.VMRunDir(id)
	logDir = b.Conf.VMLogDir(id)

	cleanup = func() {
		_ = RemoveVMDirs(runDir, logDir)
		b.RollbackCreate(ctx, id, vmCfg.Name)
	}

	if err = b.ReserveVM(ctx, id, vmCfg, blobIDs, runDir, logDir); err != nil {
		return "", "", time.Time{}, nil, fmt.Errorf("reserve VM record: %w", err)
	}
	if err = utils.EnsureDirs(runDir, logDir); err != nil {
		cleanup()
		return "", "", time.Time{}, nil, fmt.Errorf("ensure dirs: %w", err)
	}
	return runDir, logDir, now, cleanup, nil
}
