package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/creack/pty"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

// Start launches the Firecracker process for each VM ref, configures it
// via the REST API, and issues InstanceStart.
func (fc *Firecracker) Start(ctx context.Context, refs []string) ([]string, error) {
	ids, err := fc.ResolveRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	succeeded, forEachErr := fc.ForEachVM(ctx, ids, "Start", fc.startOne)
	if batchErr := fc.BatchMarkStarted(ctx, succeeded); batchErr != nil {
		log.WithFunc("firecracker.Start").Warnf(ctx, "batch state update: %v", batchErr)
	}
	return succeeded, forEachErr
}

func (fc *Firecracker) startOne(ctx context.Context, id string) error {
	rec, err := fc.LoadRecord(ctx, id)
	if err != nil {
		return err
	}

	runErr := fc.WithRunningVM(ctx, &rec, func(_ int) error { return nil })
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

	withNetwork := len(rec.NetworkConfigs) > 0
	pid, err := fc.launchProcess(ctx, &rec, sockPath, withNetwork)
	if err != nil {
		fc.MarkError(ctx, id)
		return fmt.Errorf("launch VM: %w", err)
	}

	// Configure VM via REST API and start the instance.
	if err := fc.configureVM(ctx, utils.NewSocketHTTPClient(sockPath), &rec); err != nil {
		fc.AbortLaunch(ctx, pid, sockPath, rec.RunDir, runtimeFiles)
		fc.MarkError(ctx, id)
		return fmt.Errorf("configure VM: %w", err)
	}
	return nil
}

// configureVM sends the pre-boot configuration to FC via REST API,
// then issues InstanceStart to boot the guest.
func (fc *Firecracker) configureVM(ctx context.Context, hc *http.Client, rec *hypervisor.VMRecord) error {
	memMiB := int(rec.Config.Memory >> 20) //nolint:mnd
	if err := putMachineConfig(ctx, hc, fcMachineConfig{
		VCPUCount:  rec.Config.CPU,
		MemSizeMiB: memMiB,
		HugePages:  utils.DetectHugePages(),
	}); err != nil {
		return fmt.Errorf("machine-config: %w", err)
	}

	if boot := rec.BootConfig; boot != nil {
		if err := putBootSource(ctx, hc, fcBootSource{
			KernelImagePath: boot.KernelPath,
			InitrdPath:      boot.InitrdPath,
			BootArgs:        boot.Cmdline,
		}); err != nil {
			return fmt.Errorf("boot-source: %w", err)
		}
	}

	for i, sc := range rec.StorageConfigs {
		driveID := fmt.Sprintf(driveIDFmt, i)
		if err := putDrive(ctx, hc, fcDrive{
			DriveID:      driveID,
			PathOnHost:   sc.Path,
			IsRootDevice: false,
			IsReadOnly:   sc.RO,
		}); err != nil {
			return fmt.Errorf("drive %s: %w", driveID, err)
		}
	}

	for i, nc := range rec.NetworkConfigs {
		ifaceID := fmt.Sprintf(ifaceIDFmt, i)
		if err := putNetworkInterface(ctx, hc, fcNetworkInterface{
			IfaceID:     ifaceID,
			HostDevName: nc.Tap,
			GuestMAC:    nc.Mac,
		}); err != nil {
			return fmt.Errorf("network-interface %s: %w", ifaceID, err)
		}
	}

	// Balloon: 25% of memory returned, only when memory >= 256 MiB.
	// Matches Cloud Hypervisor's balloon behavior.
	const minBalloonMemory = 256 << 20 //nolint:mnd
	const defaultBalloonDiv = 4        //nolint:mnd
	if rec.Config.Memory >= minBalloonMemory {
		balloonMiB := memMiB / defaultBalloonDiv
		if err := putBalloon(ctx, hc, fcBalloon{
			AmountMiB:         balloonMiB,
			DeflateOnOOM:      true,
			FreePageReporting: true,
		}); err != nil {
			return fmt.Errorf("balloon: %w", err)
		}
	}

	if err := instanceStart(ctx, hc); err != nil {
		return fmt.Errorf("instance-start: %w", err)
	}
	return nil
}

// launchProcess starts the firecracker binary with --api-sock,
// creates a PTY pair for the serial console, starts a background
// relay process for console.sock, writes the PID file, and waits
// for the API socket.
func (fc *Firecracker) launchProcess(ctx context.Context, rec *hypervisor.VMRecord, sockPath string, withNetwork bool) (int, error) {
	fcLog := filepath.Join(rec.LogDir, "firecracker.log")
	// FC requires the log file to exist before startup (opens O_WRONLY|O_APPEND, no O_CREATE).
	if f, createErr := os.Create(fcLog); createErr == nil { //nolint:gosec
		_ = f.Close()
	}

	// Create PTY pair: slave → FC stdin/stdout, master → console relay.
	master, slave, err := pty.Open()
	if err != nil {
		return 0, fmt.Errorf("open pty: %w", err)
	}
	defer slave.Close() //nolint:errcheck

	fcCmd := exec.Command(fc.conf.FCBinary, //nolint:gosec
		"--api-sock", sockPath,
		"--log-path", fcLog,
		"--level", "Warning",
		"--id", rec.ID,
	)
	fcCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	fcCmd.Stdin = slave
	fcCmd.Stdout = slave

	if withNetwork {
		restore, enterErr := hypervisor.EnterNetns(rec.NetworkConfigs[0].NetnsPath)
		if enterErr != nil {
			_ = master.Close()
			return 0, fmt.Errorf("enter netns: %w", enterErr)
		}
		defer restore()
	}

	if startErr := fcCmd.Start(); startErr != nil {
		_ = master.Close()
		return 0, fmt.Errorf("exec firecracker: %w", startErr)
	}
	pid := fcCmd.Process.Pid

	pidPath := fc.PIDFilePath(rec.RunDir)
	if err := utils.WritePIDFile(pidPath, pid); err != nil {
		_ = master.Close()
		_ = fcCmd.Process.Kill()
		_ = fcCmd.Wait()
		return 0, fmt.Errorf("write PID file: %w", err)
	}

	if err := hypervisor.WaitForSocket(ctx, sockPath, pid, fc.conf.SocketWaitTimeout(), fc.conf.BinaryName()); err != nil {
		_ = master.Close()
		_ = fcCmd.Process.Kill()
		_ = fcCmd.Wait()
		_ = os.Remove(pidPath)
		return 0, err
	}

	// Start console relay as a background process (self-exec).
	// The relay holds the PTY master and listens on console.sock.
	relayOK := fc.startConsoleRelay(ctx, rec.RunDir, master, pid) == nil
	if relayOK {
		// Master fd ownership transferred to relay; close parent's copy.
		_ = master.Close()
	} else {
		log.WithFunc("firecracker.launchProcess").Warn(ctx, "console relay failed (console unavailable)")
	}

	go func() {
		_ = fcCmd.Wait()
		// If relay failed, master fd was kept open to preserve ttyS0.
		// Close it now that FC has exited to avoid permanent fd leak.
		if !relayOK {
			_ = master.Close()
		}
	}()
	return pid, nil
}

// startConsoleRelay launches a background relay process that holds the PTY
// master and listens on console.sock for interactive console connections.
// The relay auto-exits when the FC process (fcPID) dies.
func (fc *Firecracker) startConsoleRelay(_ context.Context, runDir string, master *os.File, fcPID int) error {
	consoleSock := hypervisor.ConsoleSockPath(runDir)

	// Create Unix listener for console.sock.
	listener, err := net.Listen("unix", consoleSock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", consoleSock, err)
	}
	ul := listener.(*net.UnixListener)
	ul.SetUnlinkOnClose(false) // keep socket file on disk after Close
	listenerFile, err := ul.File()
	_ = ul.Close() // close Go's copy; the dup in listenerFile survives
	if err != nil {
		return fmt.Errorf("listener fd: %w", err)
	}
	defer listenerFile.Close() //nolint:errcheck

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}

	relayCmd := exec.Command(self) //nolint:gosec
	relayCmd.Env = []string{
		relayEnvKey + "=1",
		relayPIDEnvKey + "=" + fmt.Sprintf("%d", fcPID),
	}
	relayCmd.ExtraFiles = []*os.File{master, listenerFile}
	relayCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if startErr := relayCmd.Start(); startErr != nil {
		return fmt.Errorf("start relay: %w", startErr)
	}
	go relayCmd.Wait() //nolint:errcheck
	return nil
}
