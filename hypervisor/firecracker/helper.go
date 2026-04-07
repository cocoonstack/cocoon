package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	apiSockName     = "api.sock"
	pidFileName     = "fc.pid"
	consoleSockName = "console.sock"
)

var runtimeFiles = []string{apiSockName, pidFileName, consoleSockName}

func toVM(rec *hypervisor.VMRecord) *types.VM {
	info := rec.VM // value copy — detached from the DB record
	if info.State == types.VMStateRunning {
		info.SocketPath = socketPath(rec.RunDir)
		info.PID, _ = utils.ReadPIDFile(pidFile(rec.RunDir))
	}
	return &info
}

// socketPath returns the API socket path under a VM's run directory.
func socketPath(runDir string) string { return filepath.Join(runDir, apiSockName) }

// pidFile returns the PID file path under a VM's run directory.
func pidFile(runDir string) string { return filepath.Join(runDir, pidFileName) }

// consoleSockPath returns the console socket path under a VM's run directory.
func consoleSockPath(runDir string) string { return filepath.Join(runDir, consoleSockName) }

func (fc *Firecracker) resolveRef(ctx context.Context, ref string) (string, error) {
	var id string
	return id, fc.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		var err error
		id, err = idx.Resolve(ref)
		return err
	})
}

func (fc *Firecracker) resolveRefs(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	return ids, fc.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		var err error
		ids, err = idx.ResolveMany(refs)
		return err
	})
}

func (fc *Firecracker) loadRecord(ctx context.Context, id string) (hypervisor.VMRecord, error) {
	var rec hypervisor.VMRecord
	return rec, fc.store.With(ctx, func(idx *hypervisor.VMIndex) error {
		var err error
		rec, err = utils.LookupCopy(idx.VMs, id)
		return err
	})
}

func (fc *Firecracker) fcBinaryName() string {
	return filepath.Base(fc.conf.FCBinary)
}

func (fc *Firecracker) withRunningVM(ctx context.Context, rec *hypervisor.VMRecord, fn func(pid int) error) error {
	pid, pidErr := utils.ReadPIDFile(pidFile(rec.RunDir))
	if pidErr != nil && !os.IsNotExist(pidErr) {
		log.WithFunc("firecracker.withRunningVM").Warnf(ctx, "read PID file: %v", pidErr)
	}
	if !utils.VerifyProcessCmdline(pid, fc.fcBinaryName(), socketPath(rec.RunDir)) {
		return hypervisor.ErrNotRunning
	}
	return fn(pid)
}

func (fc *Firecracker) updateStates(ctx context.Context, ids []string, state types.VMState) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now()
	return fc.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
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

func (fc *Firecracker) markError(ctx context.Context, id string) {
	if err := fc.updateStates(ctx, []string{id}, types.VMStateError); err != nil {
		log.WithFunc("firecracker.markError").Warnf(ctx, "mark VM %s error: %v", id, err)
	}
}

func (fc *Firecracker) reserveVM(ctx context.Context, id string, vmCfg *types.VMConfig, blobIDs map[string]struct{}, runDir, logDir string) error {
	now := time.Now()
	return fc.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
		if idx.VMs[id] != nil {
			return fmt.Errorf("id collision %q (retry)", id)
		}
		if dup, ok := idx.Names[vmCfg.Name]; ok {
			return fmt.Errorf("vm name %q already exists (id: %s)", vmCfg.Name, dup)
		}
		idx.VMs[id] = &hypervisor.VMRecord{
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

func (fc *Firecracker) rollbackCreate(ctx context.Context, id, name string) {
	if err := fc.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
		delete(idx.VMs, id)
		if name != "" && idx.Names[name] == id {
			delete(idx.Names, name)
		}
		return nil
	}); err != nil {
		log.WithFunc("firecracker.rollbackCreate").Warnf(ctx, "rollback VM %s (name=%s): %v", id, name, err)
	}
}

func (fc *Firecracker) forEachVM(ctx context.Context, ids []string, op string, fn func(context.Context, string) error) ([]string, error) {
	logger := log.WithFunc("firecracker." + op)
	result := utils.ForEach(ctx, ids, fn, fc.conf.EffectivePoolSize())
	for _, err := range result.Errors {
		logger.Warnf(ctx, "%s: %v", op, err)
	}
	return result.Succeeded, result.Err()
}

// abortLaunch kills a FC process and removes runtime files after a failed launch.
func (fc *Firecracker) abortLaunch(ctx context.Context, pid int, sockPath, runDir string) {
	_ = utils.TerminateProcess(ctx, pid, fc.fcBinaryName(), sockPath, fc.conf.TerminateGracePeriod())
	cleanupRuntimeFiles(ctx, runDir)
}

func cleanupRuntimeFiles(ctx context.Context, runDir string) {
	for _, name := range runtimeFiles {
		p := filepath.Join(runDir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.WithFunc("firecracker.cleanupRuntimeFiles").Warnf(ctx, "cleanup %s: %v", p, err)
		}
	}
}

func removeVMDirs(runDir, logDir string) error {
	return errors.Join(
		os.RemoveAll(runDir),
		os.RemoveAll(logDir),
	)
}
