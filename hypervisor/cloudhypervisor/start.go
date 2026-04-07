package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

// Start launches the Cloud Hypervisor process for each VM ref.
// Returns the IDs that were successfully started.
func (ch *CloudHypervisor) Start(ctx context.Context, refs []string) ([]string, error) {
	ids, err := ch.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	succeeded, forEachErr := ch.ForEachVM(ctx, ids, "Start", ch.startOne)
	if batchErr := ch.BatchMarkStarted(ctx, succeeded); batchErr != nil {
		log.WithFunc("cloudhypervisor.Start").Warnf(ctx, "batch state update: %v", batchErr)
	}
	return succeeded, forEachErr
}

func (ch *CloudHypervisor) startOne(ctx context.Context, id string) error {
	rec, err := ch.LoadRecord(ctx, id)
	if err != nil {
		return err
	}

	runErr := ch.WithRunningVM(ctx, &rec, func(_ int) error { return nil })
	switch {
	case runErr == nil:
		return nil
	case errors.Is(runErr, hypervisor.ErrNotRunning):
	default:
		return fmt.Errorf("reconcile running VM %s: %w", id, runErr)
	}

	if err = utils.EnsureDirs(rec.RunDir, rec.LogDir); err != nil {
		return fmt.Errorf("ensure dirs: %w", err)
	}

	hypervisor.CleanupRuntimeFiles(ctx, rec.RunDir, runtimeFiles)

	sockPath := hypervisor.SocketPath(rec.RunDir)
	consoleSock := hypervisor.ConsoleSockPath(rec.RunDir)

	vmCfg := buildVMConfig(ctx, &rec, consoleSock)
	args := buildCLIArgs(vmCfg, sockPath)
	ch.saveCmdline(ctx, &rec, args)

	withNetwork := len(rec.NetworkConfigs) > 0
	if _, err = ch.launchProcess(ctx, &rec, sockPath, args, withNetwork); err != nil {
		ch.MarkError(ctx, id)
		return fmt.Errorf("launch VM: %w", err)
	}
	return nil
}

// launchProcess starts the cloud-hypervisor binary with the given args,
// writes the PID file, waits for the API socket to be ready, then releases
// the process handle so CH lives as an independent OS process past the
// lifetime of this binary.
func (ch *CloudHypervisor) launchProcess(ctx context.Context, rec *hypervisor.VMRecord, socketPath string, args []string, withNetwork bool) (int, error) {
	processLog := filepath.Join(rec.LogDir, "cloud-hypervisor.log")
	logFile, err := os.Create(processLog) //nolint:gosec
	if err != nil {
		log.WithFunc("cloudhypervisor.launchProcess").Warnf(ctx, "create process log: %v", err)
	} else {
		defer func() {
			if closeErr := logFile.Close(); closeErr != nil {
				log.WithFunc("cloudhypervisor.launchProcess").Warnf(ctx, "close log file: %v", closeErr)
			}
		}()
	}

	cmd := exec.Command(ch.conf.CHBinary, args...) //nolint:gosec
	// Detach from the parent process group so CH survives if this process exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	// If the VM has network, CH must be launched inside the VM's netns
	// so it can access the tap device. We setns before fork and restore after.
	if withNetwork {
		restore, enterErr := hypervisor.EnterNetns(rec.NetworkConfigs[0].NetnsPath)
		if enterErr != nil {
			return 0, fmt.Errorf("enter netns: %w", enterErr)
		}
		defer restore()
	}

	if startErr := cmd.Start(); startErr != nil {
		return 0, fmt.Errorf("exec cloud-hypervisor: %w", startErr)
	}
	pid := cmd.Process.Pid

	pidPath := ch.PIDFilePath(rec.RunDir)
	if err := utils.WritePIDFile(pidPath, pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, fmt.Errorf("write PID file: %w", err)
	}

	if err := hypervisor.WaitForSocket(ctx, socketPath, pid, ch.conf.SocketWaitTimeout(), ch.conf.BinaryName()); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(pidPath)
		return 0, err
	}

	// Reap the child process asynchronously to prevent zombies.
	// In CLI mode init adopts the orphan, but in daemon mode the
	// long-lived parent must wait() or the child becomes a zombie
	// that blocks IsProcessAlive checks during stop/delete.
	go cmd.Wait() //nolint:errcheck
	return pid, nil
}
