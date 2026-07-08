package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// StartAll runs startOne per ref; each start flips its own state under its VM's ops lock.
func (b *Backend) StartAll(ctx context.Context, refs []string, startOne func(context.Context, string) error) ([]string, error) {
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
	sockPath := SocketPath(rec.RunDir)
	pid, err := spec.Launch(ctx, rec, sockPath)
	if err != nil {
		b.MarkError(ctx, id)
		return fmt.Errorf("launch VM: %w", err)
	}
	if spec.PostLaunch != nil {
		if err := spec.PostLaunch(ctx, rec, sockPath, pid); err != nil {
			b.AbortLaunch(ctx, pid, sockPath, rec.RunDir, spec.RuntimeFiles)
			b.MarkError(ctx, id)
			return fmt.Errorf("configure VM: %w", err)
		}
	}
	// Warn-and-continue: the VMM is up and self-heals the record on the next reconcile.
	if err := b.BatchMarkStarted(ctx, []string{id}); err != nil {
		log.WithFunc(b.Typ+".StartSequence").Warnf(ctx, "mark started %s: %v", id, err)
	}
	return nil
}

// PrepareStart loads the record, verifies not-running, ensures dirs exist.
func (b *Backend) PrepareStart(ctx context.Context, id string, runtimeFiles []string) (*VMRecord, error) {
	rec, err := b.LoadRecord(ctx, id)
	if err != nil {
		return nil, err
	}

	runErr := b.WithRunningVM(ctx, &rec, func(_ int) error { return nil })
	switch {
	case runErr == nil:
		if rec.State != types.VMStateRunning {
			b.reconcileToRunning(ctx, id)
		}
		return nil, nil
	case errors.Is(runErr, ErrNotRunning):
		if hasOpenComputeInterval(&rec) {
			b.closeStaleComputeInterval(ctx, &rec)
		}
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
	defer func() {
		if err == nil {
			return
		}
		if started {
			_ = spec.Cmd.Process.Kill()
			_ = spec.Cmd.Wait()
		}
		if pidWritten {
			_ = os.Remove(spec.PIDPath)
		}
		if spec.OnFail != nil {
			spec.OnFail()
		}
	}()

	if spec.NetnsPath != "" {
		restore, nsErr := EnterNetns(spec.NetnsPath)
		if nsErr != nil {
			return 0, fmt.Errorf("enter netns: %w", nsErr)
		}
		defer restore()
	}

	if err = spec.Cmd.Start(); err != nil {
		return 0, fmt.Errorf("exec %s: %w", binaryName, err)
	}
	started = true
	pid = spec.Cmd.Process.Pid

	if err = utils.WritePIDFile(spec.PIDPath, pid); err != nil {
		return 0, fmt.Errorf("write PID file: %w", err)
	}
	pidWritten = true

	if err = WaitForSocket(ctx, spec.SockPath, pid, b.Conf.SocketWaitTimeout(), binaryName); err != nil {
		return 0, err
	}
	return pid, nil
}

// AbortLaunch terminates a failed launch and clears runtime files.
func (b *Backend) AbortLaunch(ctx context.Context, pid int, sockPath, runDir string, runtimeFiles []string) {
	_ = utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, b.Conf.TerminateGracePeriod())
	CleanupRuntimeFiles(ctx, runDir, runtimeFiles)
}
