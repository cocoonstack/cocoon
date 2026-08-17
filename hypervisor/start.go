package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"
	"golang.org/x/sys/unix"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// StartAll runs startOne per ref; each start flips its own state under its VM's ops lock.
func (b *Backend) StartAll(ctx context.Context, refs []string, startOne VMOp) ([]string, error) {
	ids, err := b.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	return b.ForEachVM(ctx, ids, "Start", startOne)
}

// StartSequence runs the shared start skeleton under the VM's ops lock: a concurrent rm --force must not delete the record/dirs mid-launch, and the Running flip lands before the lock is released so a stop queued behind this start can't be overwritten by a late state write.
func (b *Backend) StartSequence(ctx context.Context, id string, spec StartSpec) error {
	unlock, err := b.LockVMOps(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	rec, err := b.PrepareStart(ctx, id, spec.RuntimeFiles)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	if vErr := types.ValidateStorageConfigs(rec.StorageConfigs); vErr != nil {
		b.MarkError(ctx, id)
		return fmt.Errorf("storage invariants violated: %w", vErr)
	}
	if vErr := types.ValidateNetworkConfigs(rec.NetworkConfigs); vErr != nil {
		b.MarkError(ctx, id)
		return fmt.Errorf("network invariants violated: %w", vErr)
	}
	// Inside the ops lock so a concurrent stop's late quiesce cannot bring this VM's plumbing down after it is up.
	if err = b.RecoverNetwork(ctx, rec); err != nil {
		b.markFailedOperation(ctx, id, true)
		return fmt.Errorf("recover network: %w", err)
	}
	sockPath := SocketPath(rec.RunDir)
	pid, err := spec.Launch(ctx, rec, sockPath)
	if err != nil {
		b.markFailedOperation(ctx, id, true)
		return fmt.Errorf("launch VM: %w", err)
	}
	if spec.PostLaunch != nil {
		if err := spec.PostLaunch(ctx, rec, sockPath, pid); err != nil {
			b.AbortLaunch(ctx, pid, sockPath, rec.RunDir, spec.RuntimeFiles)
			b.markFailedOperation(ctx, id, true)
			return fmt.Errorf("configure VM: %w", err)
		}
	}
	// Warn-and-continue: the VMM is up and self-heals the record on the next reconcile.
	if err := b.BatchMarkStarted(ctx, []string{id}); err != nil {
		log.WithFunc(b.Typ+".StartSequence").Warnf(ctx, "mark started %s: %v", id, err)
	}
	return nil
}

// PrepareStart loads the record, refuses quarantined VMs, verifies not-running, ensures dirs exist.
func (b *Backend) PrepareStart(ctx context.Context, id string, runtimeFiles []string) (*VMRecord, error) {
	rec, err := b.EntryGuardLoad(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Quarantine != "" {
		return nil, fmt.Errorf("vm %s is quarantined (%s); recover with vm restore or delete with vm rm", id, rec.Quarantine)
	}
	if rec.State == types.VMStateCreating {
		return nil, fmt.Errorf("vm %s is still being created", id)
	}
	// Tombstone survives lost quarantine writes and process death mid-restore; the mixed run dir must not boot.
	if _, statErr := os.Stat(filepath.Join(rec.RunDir, restoreDirtyName)); statErr == nil {
		return nil, fmt.Errorf("vm %s has an interrupted restore; recover with vm restore or delete with vm rm", id)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("check restore tombstone for %s: %w", id, statErr)
	}

	runErr := b.WithRunningVM(ctx, &rec, func(_ int) error { return nil })
	switch {
	case runErr == nil:
		if rec.State != types.VMStateRunning {
			_, _ = b.ReconcileToRunning(ctx, id)
		}
		return nil, nil
	case errors.Is(runErr, ErrNotRunning):
		b.convergeCrashedStart(ctx, &rec)
	default:
		return nil, fmt.Errorf("reconcile running VM %s: %w", id, runErr)
	}

	if err = utils.EnsureDirs(rec.RunDir, rec.LogDir); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
	return &rec, nil
}

// LaunchVMProcess starts spec.Cmd and waits for the API socket; any post-Start error kills the process + removes the PID file.
func (b *Backend) LaunchVMProcess(ctx context.Context, spec LaunchSpec) (pid int, err error) {
	started := false
	pidWritten := false
	binaryName := b.Conf.BinaryName()
	pidPath := b.PIDFilePath(spec.Rec.RunDir)
	sockPath := SocketPath(spec.Rec.RunDir)
	defer func() {
		if err == nil {
			return
		}
		if started {
			_ = spec.Cmd.Process.Kill()
			_ = spec.Cmd.Wait()
		}
		if pidWritten {
			_ = os.Remove(pidPath)
		}
		if spec.OnFail != nil {
			spec.OnFail()
		}
	}()

	scope, err := cgroup.Prepare(b.Conf.CgroupParentDir(), b.Conf.CgroupCPUFence(), spec.Rec.ID, cgroup.ResolveKnobs(&spec.Rec.Config.Config), spec.DeferCPUQuota)
	if err != nil {
		return 0, fmt.Errorf("prepare cgroup scope: %w", err)
	}
	defer scope.Close() //nolint:errcheck
	setCmdCgroupFD(spec.Cmd, scope)

	if spec.NetnsPath != "" {
		restore, nsErr := EnterNetns(spec.NetnsPath)
		if nsErr != nil {
			return 0, fmt.Errorf("enter netns: %w", nsErr)
		}
		defer restore()
	}

	raiseVMMRlimits(ctx)
	if err = spec.Cmd.Start(); err != nil {
		return 0, fmt.Errorf("exec %s: %w", binaryName, err)
	}
	started = true
	pid = spec.Cmd.Process.Pid

	if err = utils.WritePIDFile(pidPath, pid); err != nil {
		return 0, fmt.Errorf("write PID file: %w", err)
	}
	pidWritten = true

	if err = WaitForSocket(ctx, sockPath, pid, b.Conf.SocketWaitTimeout(), binaryName); err != nil {
		return 0, err
	}
	return pid, nil
}

// ArmCPUQuota sets the finite quota on a scope launched with DeferCPUQuota; call after memory load, before resume.
func (b *Backend) ArmCPUQuota(id string, cfg *types.Config) error {
	if err := cgroup.Arm(b.Conf.CgroupParentDir(), id, cgroup.ResolveKnobs(cfg)); err != nil {
		return fmt.Errorf("arm cpu quota: %w", err)
	}
	return nil
}

// EffectiveCPUs resolves the host cpu set a VM under this backend may run on: explicit placement over the machine fence.
func (b *Backend) EffectiveCPUs(cfg *types.Config) []int {
	return cgroup.EffectiveCPUs(cfg.CPUSetCPUs, b.Conf.CgroupCPUFence())
}

// AbortLaunch terminates a failed launch and clears runtime files.
func (b *Backend) AbortLaunch(ctx context.Context, pid int, sockPath, runDir string, runtimeFiles []string) {
	_ = utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, b.Conf.TerminateGracePeriod())
	CleanupRuntimeFiles(ctx, runDir, runtimeFiles)
}

func raiseVMMRlimits(ctx context.Context) {
	logger := log.WithFunc("hypervisor.raiseVMMRlimits")
	inf := unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY}
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &inf); err != nil {
		logger.Warnf(ctx, "raise RLIMIT_MEMLOCK: %v", err)
	}
	var nofile unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &nofile); err == nil && nofile.Cur < nofile.Max {
		nofile.Cur = nofile.Max
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &nofile); err != nil {
			logger.Warnf(ctx, "raise RLIMIT_NOFILE: %v", err)
		}
	}
}
