//go:build linux

package utils

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// terminateWithPidfd uses pidfd_open + pidfd_send_signal for TOCTOU-safe
// process termination. Returns false if pidfd is unavailable (kernel < 5.3).
func terminateWithPidfd(pid int, binaryName, expectArg string, gracePeriod time.Duration) (handled bool, err error) {
	if !VerifyProcessCmdline(pid, binaryName, expectArg) {
		return true, nil
	}

	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false, nil
	}
	defer func() { _ = unix.Close(fd) }()

	if !VerifyProcessCmdline(pid, binaryName, expectArg) {
		return true, nil
	}

	if err := unix.PidfdSendSignal(fd, syscall.SIGTERM, nil, 0); err != nil {
		if !IsProcessAlive(pid) {
			return true, nil
		}
		return true, killFallback(pid)
	}

	ctx, cancel := context.WithTimeout(context.Background(), gracePeriod)
	defer cancel()
	if err := WaitFor(ctx, gracePeriod, 100*time.Millisecond, func() (bool, error) {
		return !IsProcessAlive(pid), nil
	}); err == nil {
		return true, nil
	}

	if err := unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0); err != nil {
		return true, killFallback(pid)
	}
	return true, WaitFor(context.Background(), killWaitTimeout, 50*time.Millisecond, func() (bool, error) {
		return !IsProcessAlive(pid), nil
	})
}

func killFallback(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	return killAndWait(context.Background(), proc, pid)
}
