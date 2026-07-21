package firecracker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/creack/pty"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Start launches FC, configures via REST, then InstanceStart.
func (fc *Firecracker) Start(ctx context.Context, refs []string) ([]string, error) {
	return fc.StartAll(ctx, refs, fc.startOne)
}

func (fc *Firecracker) startOne(ctx context.Context, id string) error {
	return fc.StartSequence(ctx, id, hypervisor.StartSpec{
		RuntimeFiles: runtimeFiles,
		Launch: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string) (int, error) {
			return fc.launchProcess(ctx, rec, sockPath, rec.ResolvedNetnsPath())
		},
		PostLaunch: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string, _ int) error {
			return fc.configureVM(ctx, utils.NewSocketHTTPClient(sockPath), rec)
		},
		// A clone symlink-redirects the source COW to its own writable disk (createDriveRedirects); launching through that window would open the clone's disk.
		Wrap: func(rec *hypervisor.VMRecord, fn func() error) error {
			return fc.withSourceWritableDisksLocked(ctx, rec.StorageConfigs, fn)
		},
	})
}

// configureVM sends pre-boot config via REST then InstanceStart.
func (fc *Firecracker) configureVM(ctx context.Context, hc *http.Client, rec *hypervisor.VMRecord) error {
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
			log.WithFunc("firecracker.configureVM").Warnf(ctx,
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

// launchProcess starts firecracker, sets up PTY+console relay, waits for socket.
func (fc *Firecracker) launchProcess(ctx context.Context, rec *hypervisor.VMRecord, sockPath, netnsPath string) (int, error) {
	fcLog := fc.LogFilePath(rec.LogDir)
	// FC opens log O_WRONLY|O_APPEND without O_CREATE — touch first.
	if f, createErr := os.Create(fcLog); createErr == nil { //nolint:gosec
		_ = f.Close()
	}

	// Create PTY pair: slave → FC stdin/stdout, master → console relay.
	master, slave, err := pty.Open()
	if err != nil {
		return 0, fmt.Errorf("open pty: %w", err)
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
		Cmd:       fcCmd,
		PIDPath:   fc.PIDFilePath(rec.RunDir),
		SockPath:  sockPath,
		NetnsPath: netnsPath,
		OnFail:    func() { _ = master.Close() },
	})
	if err != nil {
		return 0, err
	}

	relayErr := fc.startConsoleRelay(ctx, rec.RunDir, master, pid)
	if relayErr == nil {
		// Master fd ownership transferred to relay; close parent's copy.
		_ = master.Close()
	} else {
		log.WithFunc("firecracker.launchProcess").Warnf(ctx, "console relay failed (console unavailable): %v", relayErr)
	}

	go func() {
		_ = fcCmd.Wait()
		// A failed relay kept master open to preserve ttyS0; close it now that FC exited or the fd leaks forever.
		if relayErr != nil {
			_ = master.Close()
		}
	}()
	return pid, nil
}

// startConsoleRelay forks a relay that holds the PTY master, serves console.sock, and exits when fcPID dies.
func (fc *Firecracker) startConsoleRelay(_ context.Context, runDir string, master *os.File, fcPID int) error {
	consoleSock := hypervisor.ConsoleSockPath(runDir)

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

	// shell out because self-exec spawns a detached console-relay process that survives the parent.
	relayCmd := exec.Command(self) //nolint:gosec
	relayCmd.Env = []string{
		relayEnvKey + "=1",
		relayPIDEnvKey + "=" + strconv.Itoa(fcPID),
	}
	relayCmd.ExtraFiles = []*os.File{master, listenerFile}
	relayCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if startErr := relayCmd.Start(); startErr != nil {
		return fmt.Errorf("start relay: %w", startErr)
	}
	go relayCmd.Wait() //nolint:errcheck
	return nil
}
