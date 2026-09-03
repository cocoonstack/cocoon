package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const relayResponseTimeout = 5 * time.Second

func (fc *Firecracker) Start(ctx context.Context, refs []string) ([]string, error) {
	return fc.StartAll(ctx, refs, fc.startOne)
}

func (fc *Firecracker) startOne(ctx context.Context, id string) error {
	return fc.StartSequence(ctx, id, hypervisor.StartSpec{
		RuntimeFiles: runtimeFiles,
		Launch: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string) (int, error) {
			return fc.launchProcess(ctx, rec, sockPath, rec.ResolvedNetnsPath(), false)
		},
		PostLaunch: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string, _ int) error {
			return fc.configureVM(ctx, utils.NewSocketHTTPClient(sockPath), rec)
		},
	})
}

// configureVM sends pre-boot config via REST then InstanceStart.
func (fc *Firecracker) configureVM(ctx context.Context, hc *http.Client, rec *hypervisor.VMRecord) error {
	logger := log.WithFunc("firecracker.configureVM")

	memMiB := int(rec.Config.Memory >> 20) //nolint:mnd
	// No hugepages: FC's File restore backend cannot map hugetlbfs-backed snapshots, which would break hibernate/clone (#155).
	if err := putMachineConfig(ctx, hc, fcMachineConfig{
		VCPUCount:  rec.Config.CPU,
		MemSizeMiB: memMiB,
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
		if sc.Role == types.StorageRoleData && sc.DirectIO != nil {
			logger.Warnf(ctx,
				"directio on data disk %s ignored: FC has no DirectIO knob (IoEngine=Async fixed)",
				sc.Serial)
		}
		d := fcDrive{
			DriveID:      driveID,
			PathOnHost:   sc.Path,
			IsRootDevice: false,
			IsReadOnly:   sc.RO,
		}
		if !sc.RO {
			d.IoEngine = ioEngineAsync
		}
		if err := putDrive(ctx, hc, d); err != nil {
			return fmt.Errorf("drive %s: %w", driveID, err)
		}
	}

	for i, nc := range rec.NetworkConfigs {
		ifaceID := fmt.Sprintf(ifaceIDFmt, i)
		if err := putNetworkInterface(ctx, hc, fcNetworkInterface{
			IfaceID:     ifaceID,
			HostDevName: nc.TAP,
			GuestMAC:    nc.MAC,
		}); err != nil {
			return fmt.Errorf("network-interface %s: %w", ifaceID, err)
		}
	}

	if size, ok := hypervisor.BalloonSize(rec.Config.Memory, rec.Config.Windows); ok {
		if err := putBalloon(ctx, hc, fcBalloon{
			AmountMiB:         int(size >> 20), //nolint:mnd
			DeflateOnOOM:      true,
			FreePageReporting: true,
		}); err != nil {
			return fmt.Errorf("balloon: %w", err)
		}
	}

	if err := putEntropy(ctx, hc); err != nil {
		return fmt.Errorf("entropy: %w", err)
	}

	if err := putVsock(ctx, hc, fcVsock{
		GuestCID: hypervisor.VsockGuestCID,
		UDSPath:  hypervisor.VsockSockPath(rec.RunDir),
	}); err != nil {
		return fmt.Errorf("vsock: %w", err)
	}

	if err := instanceStart(ctx, hc); err != nil {
		return fmt.Errorf("instance-start: %w", err)
	}
	return nil
}

func (fc *Firecracker) launchProcess(ctx context.Context, rec *hypervisor.VMRecord, sockPath, netnsPath string, deferQuota bool) (int, error) {
	pid, _, err := fc.launchProcessWithLeases(ctx, rec, sockPath, netnsPath, nil, deferQuota)
	return pid, err
}

// launchProcessWithLeases keeps inherited source-VM leases in the console relay until the caller confirms clone setup.
func (fc *Firecracker) launchProcessWithLeases(ctx context.Context, rec *hypervisor.VMRecord, sockPath, netnsPath string, leaseFiles []*os.File, deferQuota bool) (int, *cloneLeaseControl, error) {
	logger := log.WithFunc("firecracker.launchProcessWithLeases")

	fcLog := fc.LogFilePath(rec.LogDir)
	// FC opens log O_WRONLY|O_APPEND without O_CREATE — touch first.
	if f, createErr := os.Create(fcLog); createErr == nil { //nolint:gosec
		_ = f.Close()
	}

	master, slave, err := pty.Open()
	if err != nil {
		return 0, nil, fmt.Errorf("open pty: %w", err)
	}
	defer slave.Close() //nolint:errcheck

	// shell out: the firecracker binary is the authoritative VMM.
	fcCmd := exec.Command( //nolint:gosec
		fc.conf.FCBinary,
		"--api-sock", sockPath,
		"--log-path", fcLog,
		"--level", "Warning",
		"--id", rec.ID,
	)
	fcCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	fcCmd.Stdin = slave
	fcCmd.Stdout = slave
	pid, err := fc.LaunchVMProcess(ctx, hypervisor.LaunchSpec{
		Cmd:           fcCmd,
		NetnsPath:     netnsPath,
		OnFail:        func() { _ = master.Close() },
		Rec:           rec,
		DeferCPUQuota: deferQuota,
	})
	if err != nil {
		return 0, nil, err
	}

	leaseControl, relayErr := fc.startConsoleRelay(ctx, rec.RunDir, master, pid, leaseFiles)
	switch {
	case relayErr == nil:
		// Master fd ownership transferred to relay; close parent's copy.
		_ = master.Close()
	case len(leaseFiles) > 0:
		_ = master.Close()
		fc.AbortLaunch(ctx, pid, sockPath, rec.RunDir, runtimeFiles)
		_ = fcCmd.Process.Kill()
		_ = fcCmd.Wait()
		return 0, nil, fmt.Errorf("start source-lease relay: %w", relayErr)
	default:
		logger.Warnf(ctx, "console relay failed (console unavailable): %v", relayErr)
	}

	go func() {
		_ = fcCmd.Wait()
		// A failed relay kept master open to preserve ttyS0; close it now that FC exited or the fd leaks forever.
		if relayErr != nil {
			_ = master.Close()
		}
	}()
	return pid, leaseControl, nil
}

// startConsoleRelay forks a relay that holds the PTY master, serves console.sock, and exits when fcPID dies.
func (fc *Firecracker) startConsoleRelay(_ context.Context, runDir string, master *os.File, fcPID int, leaseFiles []*os.File) (*cloneLeaseControl, error) {
	consoleSock := hypervisor.ConsoleSockPath(runDir)

	listener, err := net.Listen("unix", consoleSock)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", consoleSock, err)
	}
	ul := listener.(*net.UnixListener)
	ul.SetUnlinkOnClose(false) // keep socket file on disk after Close
	listenerFile, err := ul.File()
	_ = ul.Close() // close Go's copy; the dup in listenerFile survives
	if err != nil {
		return nil, fmt.Errorf("listener fd: %w", err)
	}
	defer listenerFile.Close() //nolint:errcheck

	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("os.Executable: %w", err)
	}
	var controlReader, controlWriter, readyReader, readyWriter *os.File
	if len(leaseFiles) > 0 {
		controlReader, controlWriter, err = os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("source-lease control pipe: %w", err)
		}
		readyReader, readyWriter, err = os.Pipe()
		if err != nil {
			closeFile(controlReader)
			closeFile(controlWriter)
			return nil, fmt.Errorf("source-lease ready pipe: %w", err)
		}
	}

	// shell out because self-exec spawns a detached console-relay process that survives the parent.
	relayCmd := exec.Command(self) //nolint:gosec
	relayCmd.Env = []string{
		relayEnvKey + "=1",
		relayPIDEnvKey + "=" + strconv.Itoa(fcPID),
		relayLeaseCountEnvKey + "=" + strconv.Itoa(len(leaseFiles)),
	}
	relayCmd.ExtraFiles = []*os.File{master, listenerFile}
	if controlReader != nil {
		relayCmd.Env = append(
			relayCmd.Env,
			relayBinaryEnvKey+"="+fc.conf.BinaryName(),
			relaySocketEnvKey+"="+hypervisor.SocketPath(runDir),
		)
		relayCmd.ExtraFiles = append(relayCmd.ExtraFiles, controlReader, readyWriter)
	}
	relayCmd.ExtraFiles = append(relayCmd.ExtraFiles, leaseFiles...)
	relayCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if startErr := relayCmd.Start(); startErr != nil {
		closeFile(controlReader)
		closeFile(controlWriter)
		closeFile(readyReader)
		closeFile(readyWriter)
		return nil, fmt.Errorf("start relay: %w", startErr)
	}
	if controlReader == nil {
		go relayCmd.Wait() //nolint:errcheck
		return nil, nil
	}
	closeFile(controlReader)
	closeFile(readyWriter)
	if readyErr := waitRelayLeaseReady(readyReader); readyErr != nil {
		closeFile(controlWriter)
		closeFile(readyReader)
		_ = relayCmd.Process.Kill()
		_ = relayCmd.Wait()
		return nil, readyErr
	}
	go relayCmd.Wait() //nolint:errcheck
	return &cloneLeaseControl{commands: controlWriter, responses: readyReader}, nil
}

func waitRelayLeaseReady(ready *os.File) error {
	return waitRelayLeaseResponse(ready, relayLeaseReadyCommand, "readiness")
}

func waitRelayLeaseResponse(response *os.File, want byte, phase string) error {
	result := make(chan error, 1)
	go func() {
		var command [1]byte
		_, err := io.ReadFull(response, command[:])
		if err == nil && command[0] != want {
			err = fmt.Errorf("unexpected response %d", command[0])
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			return fmt.Errorf("source-lease relay %s: %w", phase, err)
		}
		return nil
	case <-time.After(relayResponseTimeout):
		closeErr := response.Close()
		return errors.Join(fmt.Errorf("source-lease relay %s timed out", phase), closeErr)
	}
}

func closeFile(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}
