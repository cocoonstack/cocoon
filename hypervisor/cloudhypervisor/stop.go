package cloudhypervisor

import (
	"context"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

func (ch *CloudHypervisor) Stop(ctx context.Context, refs []string) ([]string, error) {
	return ch.StopAll(ctx, refs, ch.stopSpec())
}

func (ch *CloudHypervisor) stopOneLocked(ctx context.Context, id string) error {
	return ch.StopOneLocked(ctx, id, ch.stopSpec())
}

func (ch *CloudHypervisor) stopSpec() hypervisor.StopSpec {
	return hypervisor.StopSpec{
		RuntimeFiles: runtimeFiles,
		Shutdown: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string, pid int) error {
			if ch.conf.ForceStop() {
				return ch.forceTerminate(ctx, sockPath, pid)
			}
			hc := utils.NewSocketHTTPClient(sockPath)
			return ch.GracefulStop(ctx, rec.ID, pid, ch.conf.StopTimeout(),
				func() error { return powerButton(ctx, hc) },
				func() error { return ch.forceTerminate(ctx, sockPath, pid) },
			)
		},
	}
}

func (ch *CloudHypervisor) forceTerminate(ctx context.Context, socketPath string, pid int) error {
	return utils.TerminateProcess(ctx, pid, socketPath, vmTeardownBudget+ch.conf.TerminateGracePeriod())
}
