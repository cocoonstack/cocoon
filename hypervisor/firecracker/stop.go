package firecracker

import (
	"context"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

func (fc *Firecracker) Stop(ctx context.Context, refs []string) ([]string, error) {
	return fc.StopAll(ctx, refs, fc.stopOne)
}

func (fc *Firecracker) stopOne(ctx context.Context, id string) error {
	return fc.StopOneSequence(ctx, id, fc.stopSpec())
}

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
			hc := utils.NewSocketHTTPClient(sockPath)
			return fc.GracefulStop(ctx, rec.ID, pid, fc.conf.StopTimeout(),
				func() error { return sendCtrlAltDel(ctx, hc) },
				func() error { return fc.forceTerminate(ctx, sockPath, pid) })
		},
	}
}

func (fc *Firecracker) forceTerminate(ctx context.Context, sockPath string, pid int) error {
	return utils.TerminateProcess(ctx, pid, fc.conf.BinaryName(), sockPath, fc.conf.TerminateGracePeriod())
}
