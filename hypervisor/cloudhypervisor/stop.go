package cloudhypervisor

import (
	"context"
	"net/http"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// acpiPollInterval is how often we check if the guest has powered off
// after sending an ACPI power-button event.
const (
	acpiPollInterval = 500 * time.Millisecond
)

// Stop shuts down the Cloud Hypervisor process for each VM ref.
// Two modes are used depending on the VM's boot method:
//   - UEFI boot (cloudimg): ACPI power-button → poll → fallback SIGTERM/SIGKILL
//   - Direct boot (OCI):    vm.shutdown API → SIGTERM → SIGKILL (no ACPI)
//
// Returns the IDs that were successfully stopped.
func (ch *CloudHypervisor) Stop(ctx context.Context, refs []string) ([]string, error) {
	ids, err := ch.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	succeeded, forEachErr := ch.ForEachVM(ctx, ids, "Stop", ch.stopOne)
	if batchErr := ch.UpdateStates(ctx, succeeded, types.VMStateStopped); batchErr != nil {
		log.WithFunc("cloudhypervisor.Stop").Warnf(ctx, "batch state update: %v", batchErr)
	}
	return succeeded, forEachErr
}

func (ch *CloudHypervisor) stopOne(ctx context.Context, id string) error {
	rec, err := ch.LoadRecord(ctx, id)
	if err != nil {
		return err
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)
	hc := utils.NewSocketHTTPClient(sockPath)
	stopTimeout := time.Duration(ch.conf.StopTimeoutSeconds) * time.Second

	shutdownErr := ch.WithRunningVM(ctx, &rec, func(pid int) error {
		if isDirectBoot(rec.BootConfig) || stopTimeout < 0 /* --force */ {
			return ch.forceTerminate(ctx, hc, id, sockPath, pid)
		}
		return ch.shutdownUEFI(ctx, hc, id, sockPath, pid, stopTimeout)
	})

	return ch.HandleStopResult(ctx, id, rec.RunDir, runtimeFiles, shutdownErr)
}

// shutdownUEFI shuts down a UEFI-boot VM:
//  1. Send ACPI power-button — asks the guest OS to shut down cleanly.
//  2. Poll until the process exits or the timeout fires.
//  3. Fallback: forceTerminate (vm.shutdown → SIGTERM → SIGKILL).
func (ch *CloudHypervisor) shutdownUEFI(ctx context.Context, hc *http.Client, vmID, socketPath string, pid int, timeout time.Duration) error {
	if err := powerButton(ctx, hc); err != nil {
		log.WithFunc("cloudhypervisor.shutdownUEFI").Errorf(ctx, err, "power-button %s — falling back", vmID)
		return ch.forceTerminate(ctx, hc, vmID, socketPath, pid)
	}

	// Poll until the process exits or timeout.
	if err := utils.WaitFor(ctx, timeout, acpiPollInterval, func() (bool, error) {
		return !utils.IsProcessAlive(pid), nil
	}); err == nil {
		return nil
	}

	// Distinguish user cancellation from timeout. Ctrl-C means "abort the
	// stop operation", not "escalate to force-kill".
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Guest did not power off in time — escalate.
	log.WithFunc("cloudhypervisor.shutdownUEFI").Warnf(ctx, "VM %s did not respond to power-button within %s, escalating", vmID, timeout)
	return ch.forceTerminate(ctx, hc, vmID, socketPath, pid)
}

// forceTerminate shuts down a VM by flushing disk backends via the REST API,
// then sending SIGTERM → SIGKILL. Verifies the PID still belongs to
// cloud-hypervisor before sending signals to avoid killing a reused PID.
func (ch *CloudHypervisor) forceTerminate(ctx context.Context, hc *http.Client, vmID, socketPath string, pid int) error {
	if err := shutdownVM(ctx, hc); err != nil {
		log.WithFunc("cloudhypervisor.forceTerminate").Warnf(ctx, "vm.shutdown %s: %v", vmID, err)
	}
	return utils.TerminateProcess(ctx, pid, ch.conf.BinaryName(), socketPath, ch.conf.TerminateGracePeriod())
}

// isDirectBoot returns true when the VM was started with a direct kernel boot
// (OCI images). False means UEFI boot (cloudimg).
func isDirectBoot(boot *types.BootConfig) bool {
	return boot != nil && boot.KernelPath != ""
}
