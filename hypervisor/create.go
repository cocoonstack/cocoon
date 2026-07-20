package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// ReserveVM inserts a "creating" placeholder under id, failing on id/name collision. Re-reserving the placeholder this same create claimed via PrereserveVM adopts it (refreshing blob pins and dirs). Record birth runs under the shared GC barrier so it cannot land inside a GC cycle's snapshot window; births of different VMs share no other lock.
func (b *Backend) ReserveVM(ctx context.Context, id string, vmCfg *types.VMConfig, blobIDs map[string]struct{}, runDir, logDir string) error {
	now := time.Now()
	return b.DB.WithBirthShared(ctx, func() error {
		claimed := false
		// UpdateAny is NoDirSync: a placeholder rolled back by power failure only re-exposes resources the GC orphan sweep already reclaims.
		err := b.DB.UpdateAny(ctx, id, func(r *VMRecord, exists bool) error {
			if exists {
				if r.State == types.VMStateCreating && r.Config.Name == vmCfg.Name {
					r.ImageBlobIDs = blobIDs
					r.RunDir = runDir
					r.LogDir = logDir
					r.UpdatedAt = now
					return nil
				}
				return fmt.Errorf("id collision %q (retry)", id)
			}
			// Claim before the record write; the claim is fsynced so name uniqueness cannot split-brain across a crash.
			if err := b.DB.ClaimName(ctx, vmCfg.Name, id); err != nil {
				return err
			}
			claimed = true
			*r = VMRecord{
				VM: types.VM{
					ID: id, Hypervisor: b.Typ, State: types.VMStateCreating,
					Config: *vmCfg, CreatedAt: now, UpdatedAt: now,
				},
				ImageBlobIDs: blobIDs,
				RunDir:       runDir,
				LogDir:       logDir,
			}
			return nil
		})
		if err != nil {
			if claimed {
				// The record write failed after the claim landed: release so the name is not blocked until a repair.
				if relErr := b.DB.ReleaseName(ctx, vmCfg.Name, id); relErr != nil {
					log.WithFunc(b.Typ+".ReserveVM").Warnf(ctx, "release name %q after failed reserve: %v", vmCfg.Name, relErr)
				}
			}
			return err
		}
		// A same-name contender may have judged our claim dead inside the claim→record window and stolen it (see VMDB.ClaimName); the loser unwinds its record.
		owner, ok, err := b.DB.VerifyClaim(vmCfg.Name, id)
		if err != nil {
			return err
		}
		if !ok {
			if _, _, delErr := b.DB.Delete(ctx, id, nil); delErr != nil && !errors.Is(delErr, ErrNotFound) {
				log.WithFunc(b.Typ+".ReserveVM").Warnf(ctx, "unwind record %s after lost name race: %v", id, delErr)
			}
			return fmt.Errorf("vm name %q already exists (id: %s)", vmCfg.Name, owner)
		}
		return nil
	})
}

// PrereserveVM claims id before host resources (network) are provisioned, so GC always sees an owner for them; CreateSequence/CloneSetup later adopts the placeholder. blobIDs pins the resolved image blobs so image GC cannot sweep them pre-adoption.
func (b *Backend) PrereserveVM(ctx context.Context, id string, vmCfg *types.VMConfig, blobIDs map[string]struct{}) error {
	return b.ReserveVM(ctx, id, vmCfg, blobIDs, b.Conf.VMRunDir(id), b.Conf.VMLogDir(id))
}

// RollbackCreate removes the placeholder record and releases its name claim after a failed create.
func (b *Backend) RollbackCreate(ctx context.Context, id, name string) {
	ctx, cancel := detachedWrite(ctx)
	defer cancel()
	logger := log.WithFunc(b.Typ + ".RollbackCreate")
	if _, _, err := b.DB.Delete(ctx, id, nil); err != nil && !errors.Is(err, ErrNotFound) {
		logger.Errorf(ctx, err, "rollback VM %s (name=%s)", id, name)
	}
	if name == "" {
		return
	}
	// Record first, then claim: a crash in between leaves an orphan claim, which the next same-name create (or the GC claim sweep) repairs.
	if err := b.DB.ReleaseName(ctx, name, id); err != nil {
		logger.Errorf(ctx, err, "release name %q for VM %s", name, id)
	}
}

// FinalizeCreate persists the populated VM record (replacing the placeholder) and emits metering vm.storage.start.
func (b *Backend) FinalizeCreate(ctx context.Context, id string, info *types.VM, bootCfg *types.BootConfig, blobIDs map[string]struct{}) error {
	if err := b.DB.Update(ctx, id, func(r *VMRecord) error {
		*r = VMRecord{
			VM:           *info,
			BootConfig:   bootCfg,
			ImageBlobIDs: blobIDs,
			RunDir:       r.RunDir,
			LogDir:       r.LogDir,
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
		// Record first: dir removal deletes the held ops.lock inode, and a concurrent rm on the recreated file must not find a live placeholder.
		b.RollbackCreate(ctx, id, vmCfg.Name)
		_ = RemoveVMDirs(runDir, logDir)
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
