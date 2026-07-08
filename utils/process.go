package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const killWaitTimeout = 5 * time.Second

// WritePIDFile writes pid to path with 0600 permissions.
func WritePIDFile(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// ReadPIDFile reads a PID integer from path.
func ReadPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // internal runtime path
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse PID from %s: %w", path, err)
	}
	return pid, nil
}

// IsProcessAlive uses kill(pid, 0) — true if the process exists; EPERM still counts (process exists, lacking signal permission).
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// VerifyProcessCmdline matches pid against binaryName + expectArg in /proc/<pid>/cmdline; falls back to IsProcessAlive on non-Linux or read errors.
func VerifyProcessCmdline(pid int, binaryName, expectArg string) bool {
	if pid <= 0 {
		return false
	}
	if match, err := verifyProcessCmdline(pid, binaryName, expectArg); err == nil {
		return match
	}
	return IsProcessAlive(pid)
}

// TerminateProcess verifies pid matches binaryName+expectArg, sends SIGTERM, waits gracePeriod, escalates to SIGKILL.
func TerminateProcess(ctx context.Context, pid int, binaryName, expectArg string, gracePeriod time.Duration) error {
	if handled, err := terminateWithPidfd(ctx, pid, binaryName, expectArg, gracePeriod); handled {
		return err
	}

	if !VerifyProcessCmdline(pid, binaryName, expectArg) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if !IsProcessAlive(pid) {
			return nil
		}
		return killAndWait(ctx, proc, pid)
	}

	if err := waitDead(ctx, pid, gracePeriod); err == nil {
		return nil
	}

	return killAndWait(ctx, proc, pid)
}

func killAndWait(ctx context.Context, proc *os.Process, pid int) error {
	_ = proc.Kill()
	return waitDead(ctx, pid, killWaitTimeout)
}

// waitDead polls until pid no longer responds to kill(0), bounded by timeout.
func waitDead(ctx context.Context, pid int, timeout time.Duration) error {
	return WaitFor(ctx, timeout, time.Millisecond, func() (bool, error) {
		return !IsProcessAlive(pid), nil
	})
}
