package cloudhypervisor

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"github.com/projecteru2/core/log"
	"golang.org/x/sys/unix"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func (ch *CloudHypervisor) Start(ctx context.Context, refs []string) ([]string, error) {
	return ch.StartAll(ctx, refs, ch.startOne)
}

func (ch *CloudHypervisor) startOne(ctx context.Context, id string) error {
	return ch.StartSequence(ctx, id, hypervisor.StartSpec{
		RuntimeFiles: runtimeFiles,
		Launch: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string) (int, error) {
			vmCfg := buildVMConfig(ctx, rec, hypervisor.ConsoleSockPath(rec.RunDir))
			args := buildCLIArgs(vmCfg, sockPath)
			ch.saveCmdline(ctx, rec, args)
			return ch.launchProcess(ctx, rec, sockPath, args, rec.ResolvedNetnsPath())
		},
	})
}

func (ch *CloudHypervisor) launchProcess(ctx context.Context, rec *hypervisor.VMRecord, socketPath string, args []string, netnsPath string) (int, error) {
	processLog := ch.LogFilePath(rec.LogDir)
	logFile, err := os.Create(processLog) //nolint:gosec
	if err != nil {
		log.WithFunc("cloudhypervisor.launchProcess").Warnf(ctx, "create process log: %v", err)
	} else {
		defer logFile.Close() //nolint:errcheck
	}

	raiseVMMRlimits(ctx)
	// shell out: the cloud-hypervisor binary is the authoritative VMM.
	cmd := exec.Command(ch.conf.CHBinary, args...) //nolint:gosec
	// Setpgid so CH survives if this process exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	pid, err := ch.LaunchVMProcess(ctx, hypervisor.LaunchSpec{
		Cmd:       cmd,
		PIDPath:   ch.PIDFilePath(rec.RunDir),
		SockPath:  socketPath,
		NetnsPath: netnsPath,
	})
	if err != nil {
		return 0, err
	}

	// Daemon mode: parent must wait() or zombie blocks IsProcessAlive on stop/delete.
	go cmd.Wait() //nolint:errcheck
	return pid, nil
}

func raiseVMMRlimits(ctx context.Context) {
	logger := log.WithFunc("cloudhypervisor.raiseVMMRlimits")
	inf := unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY}
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &inf); err != nil {
		logger.Warnf(ctx, "raise RLIMIT_MEMLOCK: %v", err)
	}
	var nofile unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &nofile); err == nil && nofile.Cur < nofile.Max {
		nofile.Cur = nofile.Max
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &nofile); err != nil {
			logger.Warnf(ctx, "raise RLIMIT_NOFILE: %v", err)
		}
	}
}
