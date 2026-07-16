package cloudhypervisor

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func (ch *CloudHypervisor) Start(ctx context.Context, refs []string) ([]string, error) {
	return ch.StartAll(ctx, refs, ch.startOne)
}

func (ch *CloudHypervisor) startOne(ctx context.Context, id string) error {
	return ch.StartSequence(ctx, id, hypervisor.StartSpec{
		RuntimeFiles: runtimeFiles,
		Launch: func(ctx context.Context, rec *hypervisor.VMRecord, sockPath string) (int, error) {
			vmCfg := buildVMConfig(rec, hypervisor.ConsoleSockPath(rec.RunDir))
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
