package firecracker

import (
	"context"
	"net/http"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

// Stop shuts down each FC: SendCtrlAltDel + --timeout wait, or --force for immediate kill.
func (fc *Firecracker) Stop(ctx context.Context, refs []string) ([]string, error) {
	return fc.StopAll(ctx, refs, fc.stopOne)
}

func (fc *Firecracker) stopOne(ctx context.Context, id string) error {
	return fc.StopOneSequence(ctx, id, fc.stopSpec())
}

// stopOneLocked is stopOne for callers already holding the VM's ops lock (DeleteAll).
func (fc *Firecracker) stopOneLocked(ctx context.Context, id string) error {
	return fc.StopOneLocked(ctx, id, fc.stopSpec())
}

func (fc *Firecracker) stopSpec() hypervisor.StopSpec {
	return hypervisor.StopSpec{
		RuntimeFiles: runtimeFiles,
		Shutdown: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string, pid int) error {
			if fc.conf.ForceStop() {
				return fc.forceTerminate(ctx, sockPath, pid)
			}
			return fc.gracefulStop(ctx, utils.NewSocketHTTPClient(sockPath), rec.ID, sockPath, pid, fc.conf.StopTimeout())
		},
	}
}

func (fc *Firecracker) gracefulStop(ctx context.Context, hc *http.Client, vmID, sockPath string, pid int, timeout time.Duration) error {
	return fc.GracefulStop(
		ctx, vmID, pid, timeout,
		func() error { return sendCtrlAltDel(ctx, hc) },
		func() error { return fc.forceTerminate(ctx, sockPath, pid) },
	)
}

// forceTerminate skips graceful shutdown, going straight to SIGTERM → SIGKILL.
func (fc *Firecracker) forceTerminate(ctx context.Context, sockPath string, pid int) error {
	return utils.TerminateProcess(ctx, pid, fc.conf.BinaryName(), sockPath, fc.conf.TerminateGracePeriod())
}
