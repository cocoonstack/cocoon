package firecracker

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const ctrlAltDelPollInterval = 500 * time.Millisecond

// Stop shuts down the Firecracker process for each VM ref.
// Honors --force (skip SendCtrlAltDel, immediate kill) and --timeout
// (wait for guest to respond to SendCtrlAltDel before escalating).
func (fc *Firecracker) Stop(ctx context.Context, refs []string) ([]string, error) {
	ids, err := fc.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	succeeded, forEachErr := fc.ForEachVM(ctx, ids, "Stop", fc.stopOne)
	if batchErr := fc.UpdateStates(ctx, succeeded, types.VMStateStopped); batchErr != nil {
		log.WithFunc("firecracker.Stop").Warnf(ctx, "batch state update: %v", batchErr)
	}
	return succeeded, forEachErr
}

func (fc *Firecracker) stopOne(ctx context.Context, id string) error {
	rec, err := fc.LoadRecord(ctx, id)
	if err != nil {
		return err
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)
	hc := utils.NewSocketHTTPClient(sockPath)
	stopTimeout := time.Duration(fc.conf.StopTimeoutSeconds) * time.Second

	shutdownErr := fc.WithRunningVM(ctx, &rec, func(pid int) error {
		// --force (StopTimeoutSeconds < 0): skip SendCtrlAltDel, immediate kill.
		if stopTimeout < 0 {
			return fc.forceTerminate(ctx, sockPath, pid)
		}
		return fc.gracefulStop(ctx, hc, id, sockPath, pid, stopTimeout)
	})

	switch {
	case errors.Is(shutdownErr, hypervisor.ErrNotRunning):
		hypervisor.CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
		return nil
	case shutdownErr != nil:
		fc.MarkError(ctx, id)
		return shutdownErr
	default:
		hypervisor.CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)
		return nil
	}
}

// gracefulStop sends SendCtrlAltDel, waits for the guest to shut down within
// the timeout, then falls back to forceTerminate.
func (fc *Firecracker) gracefulStop(ctx context.Context, hc *http.Client, vmID, sockPath string, pid int, timeout time.Duration) error {
	if err := sendCtrlAltDel(ctx, hc); err != nil {
		log.WithFunc("firecracker.gracefulStop").Warnf(ctx, "SendCtrlAltDel %s: %v — escalating", vmID, err)
		return fc.forceTerminate(ctx, sockPath, pid)
	}

	// Poll until the process exits or timeout.
	if err := utils.WaitFor(ctx, timeout, ctrlAltDelPollInterval, func() (bool, error) {
		return !utils.IsProcessAlive(pid), nil
	}); err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	log.WithFunc("firecracker.gracefulStop").Warnf(ctx, "VM %s did not respond to SendCtrlAltDel within %s, escalating", vmID, timeout)
	return fc.forceTerminate(ctx, sockPath, pid)
}

// forceTerminate skips graceful shutdown, going straight to SIGTERM → SIGKILL.
func (fc *Firecracker) forceTerminate(ctx context.Context, sockPath string, pid int) error {
	return utils.TerminateProcess(ctx, pid, fc.conf.BinaryName(), sockPath, fc.conf.TerminateGracePeriod())
}
