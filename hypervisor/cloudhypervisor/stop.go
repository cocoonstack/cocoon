package cloudhypervisor

import (
	"context"
	"net/http"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

// Stop shuts down each CH process: UEFI uses ACPI power-button; direct-boot uses vm.shutdown. Both fall back to SIGTERM→SIGKILL.
func (ch *CloudHypervisor) Stop(ctx context.Context, refs []string) ([]string, error) {
	return ch.StopAll(ctx, refs, ch.stopOne)
}

func (ch *CloudHypervisor) stopOne(ctx context.Context, id string) error {
	return ch.StopOneSequence(ctx, id, ch.stopSpec())
}

// stopOneLocked is stopOne for callers already holding the VM's ops lock (DeleteAll).
func (ch *CloudHypervisor) stopOneLocked(ctx context.Context, id string) error {
	return ch.StopOneLocked(ctx, id, ch.stopSpec())
}

func (ch *CloudHypervisor) stopSpec() hypervisor.StopSpec {
	return hypervisor.StopSpec{
		RuntimeFiles: runtimeFiles,
		Shutdown: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string, pid int) error {
			hc := utils.NewSocketHTTPClient(sockPath)
			if hypervisor.IsDirectBoot(rec.BootConfig) || ch.conf.ForceStop() {
				return ch.forceTerminate(ctx, hc, rec.ID, sockPath, pid)
			}
			return ch.shutdownUEFI(ctx, hc, rec.ID, sockPath, pid, ch.conf.StopTimeout())
		},
	}
}

// shutdownUEFI shuts down a UEFI-boot VM via ACPI power-button with poll-and-escalate handled by the shared GracefulStop helper.
func (ch *CloudHypervisor) shutdownUEFI(ctx context.Context, hc *http.Client, vmID, socketPath string, pid int, timeout time.Duration) error {
	return ch.GracefulStop(ctx, vmID, pid, timeout,
		func() error { return powerButton(ctx, hc) },
		func() error { return ch.forceTerminate(ctx, hc, vmID, socketPath, pid) },
	)
}

// forceTerminate flushes disks via REST then SIGTERM→SIGKILL; verifies pid is still cloud-hypervisor to avoid signaling a reused PID.
func (ch *CloudHypervisor) forceTerminate(ctx context.Context, hc *http.Client, vmID, socketPath string, pid int) error {
	if err := shutdownVM(ctx, hc); err != nil {
		log.WithFunc("cloudhypervisor.forceTerminate").Warnf(ctx, "vm.shutdown %s: %v", vmID, err)
	}
	return utils.TerminateProcess(ctx, pid, ch.conf.BinaryName(), socketPath, ch.conf.TerminateGracePeriod())
}
