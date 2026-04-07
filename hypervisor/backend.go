package hypervisor

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Shared constants for all hypervisor backends.
const (
	APISocketName   = "api.sock"
	ConsoleSockName = "console.sock"

	// CowSerial is the well-known virtio serial for the COW disk attached to OCI VMs.
	CowSerial = "cocoon-cow"

	// CreatingStateGCGrace is how long a VM can stay in "creating" state
	// before GC treats it as a crash remnant and cleans it up.
	CreatingStateGCGrace = 24 * time.Hour
)

// BackendConfig provides backend-specific values needed by shared Backend methods.
type BackendConfig interface {
	BinaryName() string
	PIDFileName() string
	TerminateGracePeriod() time.Duration
	EffectivePoolSize() int
}

// Backend provides shared store operations for hypervisor backends.
// Embed this struct in backend implementations to avoid duplicating
// store access patterns (resolve, load, state updates, VM iteration).
type Backend struct {
	Typ    string
	Conf   BackendConfig
	DB     storage.Store[VMIndex]
	Locker lock.Locker
}

// Type returns the backend identifier (e.g., "cloud-hypervisor", "firecracker").
func (b *Backend) Type() string { return b.Typ }

// Inspect returns VM info for a single VM by ref (ID, name, or prefix).
func (b *Backend) Inspect(ctx context.Context, ref string) (*types.VM, error) {
	var result *types.VM
	return result, b.DB.With(ctx, func(idx *VMIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		result = b.ToVM(idx.VMs[id])
		return nil
	})
}

// List returns VM info for all known VMs.
func (b *Backend) List(ctx context.Context) ([]*types.VM, error) {
	var result []*types.VM
	return result, b.DB.With(ctx, func(idx *VMIndex) error {
		for _, rec := range idx.VMs {
			if rec == nil {
				continue
			}
			result = append(result, b.ToVM(rec))
		}
		return nil
	})
}

// ToVM converts a VMRecord to a types.VM with runtime fields populated.
// Deep-copies SnapshotIDs to prevent shared mutable reference to DB record.
func (b *Backend) ToVM(rec *VMRecord) *types.VM {
	info := rec.VM // value copy
	if info.State == types.VMStateRunning {
		info.SocketPath = SocketPath(rec.RunDir)
		info.PID, _ = utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
	}
	if info.SnapshotIDs != nil {
		ids := make(map[string]struct{}, len(info.SnapshotIDs))
		maps.Copy(ids, info.SnapshotIDs)
		info.SnapshotIDs = ids
	}
	return &info
}

// ResolveRef resolves a single ref (ID, name, or prefix) to an exact VM ID.
func (b *Backend) ResolveRef(ctx context.Context, ref string) (string, error) {
	var id string
	return id, b.DB.With(ctx, func(idx *VMIndex) error {
		var err error
		id, err = idx.Resolve(ref)
		return err
	})
}

// ResolveRefs batch-resolves refs to exact VM IDs under a single lock.
func (b *Backend) ResolveRefs(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	return ids, b.DB.With(ctx, func(idx *VMIndex) error {
		var err error
		ids, err = idx.ResolveMany(refs)
		return err
	})
}

// LoadRecord loads a deep copy of a VM record by ID.
func (b *Backend) LoadRecord(ctx context.Context, id string) (VMRecord, error) {
	var rec VMRecord
	return rec, b.DB.With(ctx, func(idx *VMIndex) error {
		var err error
		rec, err = utils.LookupCopy(idx.VMs, id)
		return err
	})
}

// WithRunningVM verifies the VM process is alive, then calls fn with the PID.
// Returns ErrNotRunning if the process is not alive.
func (b *Backend) WithRunningVM(ctx context.Context, rec *VMRecord, fn func(pid int) error) error {
	pid, pidErr := utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
	if pidErr != nil && !os.IsNotExist(pidErr) {
		log.WithFunc(b.Typ+".WithRunningVM").Warnf(ctx, "read PID file: %v", pidErr)
	}
	if !utils.VerifyProcessCmdline(pid, b.Conf.BinaryName(), SocketPath(rec.RunDir)) {
		return ErrNotRunning
	}
	return fn(pid)
}

// UpdateStates updates the state and timestamp for a batch of VM IDs.
func (b *Backend) UpdateStates(ctx context.Context, ids []string, state types.VMState) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now()
	return b.DB.Update(ctx, func(idx *VMIndex) error {
		for _, id := range ids {
			r := idx.VMs[id]
			if r == nil {
				continue
			}
			r.State = state
			r.UpdatedAt = now
			switch state {
			case types.VMStateRunning:
				r.StartedAt = &now
			case types.VMStateStopped:
				r.StoppedAt = &now
			}
		}
		return nil
	})
}

// MarkError marks a VM as error state. Logs but does not return errors.
func (b *Backend) MarkError(ctx context.Context, id string) {
	if err := b.UpdateStates(ctx, []string{id}, types.VMStateError); err != nil {
		log.WithFunc(b.Typ+".MarkError").Warnf(ctx, "mark VM %s error: %v", id, err)
	}
}

// ReserveVM writes a placeholder VMRecord (state=Creating) so GC won't
// treat the VM's directories as orphans.
func (b *Backend) ReserveVM(ctx context.Context, id string, vmCfg *types.VMConfig, blobIDs map[string]struct{}, runDir, logDir string) error {
	now := time.Now()
	return b.DB.Update(ctx, func(idx *VMIndex) error {
		if idx.VMs[id] != nil {
			return fmt.Errorf("id collision %q (retry)", id)
		}
		if dup, ok := idx.Names[vmCfg.Name]; ok {
			return fmt.Errorf("vm name %q already exists (id: %s)", vmCfg.Name, dup)
		}
		idx.VMs[id] = &VMRecord{
			VM: types.VM{
				ID: id, State: types.VMStateCreating,
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

// RollbackCreate removes a placeholder VM record from the DB.
func (b *Backend) RollbackCreate(ctx context.Context, id, name string) {
	if err := b.DB.Update(ctx, func(idx *VMIndex) error {
		delete(idx.VMs, id)
		if name != "" && idx.Names[name] == id {
			delete(idx.Names, name)
		}
		return nil
	}); err != nil {
		log.WithFunc(b.Typ+".RollbackCreate").Warnf(ctx, "rollback VM %s (name=%s): %v", id, name, err)
	}
}

// ForEachVM runs fn for each ID concurrently (bounded by PoolSize).
func (b *Backend) ForEachVM(ctx context.Context, ids []string, op string, fn func(context.Context, string) error) ([]string, error) {
	logger := log.WithFunc(b.Typ + "." + op)
	result := utils.ForEach(ctx, ids, fn, b.Conf.EffectivePoolSize())
	for _, err := range result.Errors {
		logger.Warnf(ctx, "%s: %v", op, err)
	}
	return result.Succeeded, result.Err()
}

// AbortLaunch kills a hypervisor process and removes runtime files after a failed launch.
func (b *Backend) AbortLaunch(ctx context.Context, pid int, sockPath, runDir string, runtimeFiles []string) {
	_ = utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, b.Conf.TerminateGracePeriod())
	CleanupRuntimeFiles(ctx, runDir, runtimeFiles)
}

// BatchMarkStarted updates a batch of VMs to Running state with FirstBooted=true.
func (b *Backend) BatchMarkStarted(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now()
	return b.DB.Update(ctx, func(idx *VMIndex) error {
		for _, id := range ids {
			r := idx.VMs[id]
			if r == nil {
				continue
			}
			r.State = types.VMStateRunning
			r.StartedAt = &now
			r.UpdatedAt = now
			r.FirstBooted = true
		}
		return nil
	})
}

// CleanStalePlaceholders removes DB records stuck in "creating" state
// past the GC grace period. Used by GC Collect phase.
func (b *Backend) CleanStalePlaceholders(_ context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-CreatingStateGCGrace)
	return b.DB.WriteRaw(func(idx *VMIndex) error {
		utils.CleanStaleRecords(idx.VMs, idx.Names, ids,
			func(r *VMRecord) string { return r.Config.Name },
			func(r *VMRecord) bool {
				return r.State == types.VMStateCreating && r.UpdatedAt.Before(cutoff)
			},
		)
		return nil
	})
}

// SocketPath returns the API socket path under a VM's run directory.
func SocketPath(runDir string) string { return filepath.Join(runDir, APISocketName) }

// PIDFilePath returns the PID file path for the backend's PID file name.
func (b *Backend) PIDFilePath(runDir string) string {
	return filepath.Join(runDir, b.Conf.PIDFileName())
}

// ConsoleSockPath returns the console socket path under a VM's run directory.
func ConsoleSockPath(runDir string) string { return filepath.Join(runDir, ConsoleSockName) }
