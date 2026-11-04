package firecracker

import (
	"context"
	"errors"
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

func (fc *Firecracker) Start(ctx context.Context, refs []string) ([]string, error) {
	return fc.StartAll(ctx, refs, fc.startOne)
}

func (fc *Firecracker) startOne(ctx context.Context, id string, scan *utils.ProcScan) error {
	return fc.StartSequence(ctx, id, scan, hypervisor.StartSpec{
		RuntimeFiles: runtimeFiles,
		Launch: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string) (int, error) {
			return fc.launchProcess(ctx, rec, sockPath, rec.ResolvedNetnsPath(), false)
		},
		PostLaunch: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string, _ int) error {
			return fc.configureVM(ctx, utils.NewSocketHTTPClient(sockPath), rec)
		},
		ConvergeRunning: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string, _ int) error {
			hc := utils.NewSocketHTTPClient(sockPath)
			err := convergeOrphanedPause(ctx, hc, rec.ID)
			if errors.Is(err, hypervisor.ErrNotRunning) {
				return fc.configureVM(ctx, hc, rec)
			}
			return err
		},
	})
}

func (fc *Firecracker) configureVM(ctx context.Context, hc *http.Client, rec *hypervisor.VMRecord) error {
	logger := log.WithFunc("firecracker.configureVM")

	if err := putJSON(ctx, hc, "/machine-config", buildMachineConfig(rec), "machine-config"); err != nil {
		return fmt.Errorf("machine-config: %w", err)
	}

	if boot := rec.BootConfig; boot != nil {
		dns, err := fc.conf.DNSServers()
		if err != nil {
			return fmt.Errorf("parse DNS servers: %w", err)
		}
		if err := putJSON(ctx, hc, "/boot-source", fcBootSource{
			KernelImagePath: boot.KernelPath,
			InitrdPath:      boot.InitrdPath,
			BootArgs:        buildCmdline(rec.StorageConfigs, rec.NetworkConfigs, rec.Config.Name, dns),
		}, "boot-source"); err != nil {
			return fmt.Errorf("boot-source: %w", err)
		}
	}

	for i, sc := range rec.StorageConfigs {
		driveID := fmt.Sprintf(driveIDFmt, i)
		if sc.Role == types.StorageRoleData && sc.DirectIO != nil {
			logger.Warnf(ctx, directIOIgnoredMsg, sc.Serial)
		}
		d := fcDrive{
			DriveID:    driveID,
			PathOnHost: sc.Path,
			IsReadOnly: sc.RO,
		}
		if !sc.RO {
			d.IoEngine = ioEngineAsync
		}
		if err := putJSON(ctx, hc, "/drives/"+driveID, d, "drive"); err != nil {
			return fmt.Errorf("drive %s: %w", driveID, err)
		}
	}

	for i, nc := range rec.NetworkConfigs {
		ifaceID := fmt.Sprintf(ifaceIDFmt, i)
		if err := putJSON(ctx, hc, "/network-interfaces/"+ifaceID, fcNetworkInterface{
			IfaceID:     ifaceID,
			HostDevName: nc.TAP,
			GuestMAC:    nc.MAC,
			MTU:         nc.MTU,
		}, "network-interface"); err != nil {
			return fmt.Errorf("network-interface %s: %w", ifaceID, err)
		}
	}

	if size, ok := hypervisor.BalloonSize(rec.Config.Config); ok {
		if err := putJSON(ctx, hc, "/balloon", fcBalloon{
			AmountMiB:         int(size >> 20),
			DeflateOnOOM:      true,
			FreePageReporting: true,
		}, "balloon"); err != nil {
			return fmt.Errorf("balloon: %w", err)
		}
	}

	if err := putJSON(ctx, hc, "/entropy", struct{}{}, "entropy"); err != nil {
		return fmt.Errorf("entropy: %w", err)
	}

	if err := putJSON(ctx, hc, "/vsock", fcVsock{
		GuestCID: hypervisor.VsockGuestCID,
		UDSPath:  hypervisor.VsockSockPath(rec.RunDir),
	}, "vsock"); err != nil {
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
	// FC opens its log O_WRONLY|O_APPEND without O_CREATE, so cocoon creates it; appending keeps the previous launch's tail.
	if f, createErr := os.OpenFile(fcLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); createErr == nil { //nolint:gosec
		_ = f.Close()
	}

	master, slave, err := pty.Open()
	if err != nil {
		return 0, nil, fmt.Errorf("open pty: %w", err)
	}
	defer slave.Close() //nolint:errcheck

	// Shell out: the firecracker binary is the authoritative VMM.
	fcCmd := exec.Command(fc.conf.FCBinary, launchArgs(rec, sockPath, fcLog)...) //nolint:gosec
	fcCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	fcCmd.Stdin = slave
	fcCmd.Stdout = slave
	pid, exited, err := fc.LaunchVMProcess(ctx, hypervisor.LaunchSpec{
		Cmd:           fcCmd,
		NetnsPath:     netnsPath,
		Rec:           rec,
		DeferCPUQuota: deferQuota,
	})
	if err != nil {
		_ = master.Close()
		return 0, nil, err
	}

	leaseControl, relayErr := fc.startConsoleRelay(rec.RunDir, master, pid, leaseFiles)
	switch {
	case relayErr == nil:
		_ = master.Close()
	case len(leaseFiles) > 0:
		_ = master.Close()
		fc.AbortLaunch(ctx, pid, sockPath, rec.RunDir, runtimeFiles)
		_ = fcCmd.Process.Kill()
		<-exited
		return 0, nil, fmt.Errorf("start source-lease relay: %w", relayErr)
	default:
		// The socket file outlives a failed relay and would make inspect report a console nobody serves.
		_ = os.Remove(hypervisor.ConsoleSockPath(rec.RunDir))
		logger.Warnf(ctx, "console relay failed (console unavailable): %v", relayErr)
	}
	return pid, leaseControl, nil
}

// startConsoleRelay forks a relay that holds the PTY master, serves console.sock, and exits when fcPID dies.
func (fc *Firecracker) startConsoleRelay(runDir string, master *os.File, fcPID int, leaseFiles []*os.File) (*cloneLeaseControl, error) {
	consoleSock := hypervisor.ConsoleSockPath(runDir)

	listener, err := net.Listen("unix", consoleSock)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", consoleSock, err)
	}
	ul := listener.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
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
			_ = controlReader.Close()
			_ = controlWriter.Close()
			return nil, fmt.Errorf("source-lease ready pipe: %w", err)
		}
	}

	// Shell out because self-exec spawns a detached console-relay process that survives the parent.
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
			relaySocketEnvKey+"="+hypervisor.SocketPath(runDir),
		)
		relayCmd.ExtraFiles = append(relayCmd.ExtraFiles, controlReader, readyWriter)
	}
	relayCmd.ExtraFiles = append(relayCmd.ExtraFiles, leaseFiles...)
	relayCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if startErr := relayCmd.Start(); startErr != nil {
		_ = controlReader.Close()
		_ = controlWriter.Close()
		_ = readyReader.Close()
		_ = readyWriter.Close()
		return nil, fmt.Errorf("start relay: %w", startErr)
	}
	if controlReader == nil {
		go relayCmd.Wait() //nolint:errcheck
		return nil, nil
	}
	_ = controlReader.Close()
	_ = readyWriter.Close()
	if readyErr := waitRelayLeaseReady(readyReader); readyErr != nil {
		_ = controlWriter.Close()
		_ = readyReader.Close()
		_ = relayCmd.Process.Kill()
		_ = relayCmd.Wait()
		return nil, readyErr
	}
	go relayCmd.Wait() //nolint:errcheck
	return &cloneLeaseControl{commands: controlWriter, responses: readyReader}, nil
}

func launchArgs(rec *hypervisor.VMRecord, sockPath, fcLog string) []string {
	args := []string{"--api-sock", sockPath, "--log-path", fcLog, "--level", "Warning", "--id", rec.ID}
	if rec.Config.PCI {
		args = append(args, "--enable-pci")
	}
	return args
}

// Firecracker rejects pcie_hotplug_ports above 0 on an MMIO VM.
func buildMachineConfig(rec *hypervisor.VMRecord) fcMachineConfig {
	cfg := fcMachineConfig{VCPUCount: rec.Config.CPU, MemSizeMiB: int(rec.Config.Memory >> 20)}
	if rec.Config.PCI {
		cfg.PCIeHotplugPorts = pcieHotplugPorts
	}
	return cfg
}
