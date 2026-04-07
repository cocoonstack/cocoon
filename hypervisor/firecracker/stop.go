package firecracker

import (
	"context"
	"errors"
	"net/http"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Stop shuts down the Firecracker process for each VM ref.
// FC direct-boot VMs use SendCtrlAltDel → SIGTERM → SIGKILL.
// Returns the IDs that were successfully stopped.
func (fc *Firecracker) Stop(ctx context.Context, refs []string) ([]string, error) {
	ids, err := fc.resolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	succeeded, forEachErr := fc.forEachVM(ctx, ids, "Stop", fc.stopOne)
	if batchErr := fc.updateStates(ctx, succeeded, types.VMStateStopped); batchErr != nil {
		log.WithFunc("firecracker.Stop").Warnf(ctx, "batch state update: %v", batchErr)
	}
	return succeeded, forEachErr
}

func (fc *Firecracker) stopOne(ctx context.Context, id string) error {
	rec, err := fc.loadRecord(ctx, id)
	if err != nil {
		return err
	}

	sockPath := socketPath(rec.RunDir)
	hc := utils.NewSocketHTTPClient(sockPath)

	shutdownErr := fc.withRunningVM(ctx, &rec, func(pid int) error {
		return fc.forceTerminate(ctx, hc, id, sockPath, pid)
	})

	switch {
	case errors.Is(shutdownErr, hypervisor.ErrNotRunning):
		cleanupRuntimeFiles(ctx, rec.RunDir)
		return nil
	case shutdownErr != nil:
		fc.markError(ctx, id)
		return shutdownErr
	default:
		cleanupRuntimeFiles(ctx, rec.RunDir)
		return nil
	}
}

// forceTerminate sends SendCtrlAltDel via the FC API (graceful guest shutdown),
// then falls back to SIGTERM → SIGKILL.
func (fc *Firecracker) forceTerminate(ctx context.Context, hc *http.Client, vmID, sockPath string, pid int) error {
	if err := sendCtrlAltDel(ctx, hc); err != nil {
		log.WithFunc("firecracker.forceTerminate").Warnf(ctx, "SendCtrlAltDel %s: %v", vmID, err)
	}
	return utils.TerminateProcess(ctx, pid, fc.fcBinaryName(), sockPath, fc.conf.TerminateGracePeriod())
}
