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

// WritePIDFile writes pid to path atomically.
func WritePIDFile(path string, pid int) error {
	return AtomicWriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600, NoSync)
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

// IsProcessAlive treats EPERM as alive: the process exists, this one lacks signal permission.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// VerifyProcessCmdline matches pid against a whole expectArg argument in /proc/<pid>/cmdline.
func VerifyProcessCmdline(pid int, expectArg string) bool {
	if pid <= 0 {
		return false
	}
	if match, err := verifyProcessCmdline(pid, expectArg); err == nil {
		return match
	}
	return IsProcessAlive(pid)
}

// TerminateProcess verifies pid matches expectArg, sends SIGTERM, waits gracePeriod, escalates to SIGKILL.
func TerminateProcess(ctx context.Context, pid int, expectArg string, gracePeriod time.Duration) error {
	if handled, err := terminateWithPidfd(ctx, pid, expectArg, gracePeriod); handled {
		return err
	}

	match, err := verifyProcessForTermination(pid, expectArg)
	if err != nil {
		return err
	}
	if !match {
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
		return killAndWait(ctx, proc, pid, expectArg)
	}

	if err := waitDead(ctx, pid, gracePeriod); err == nil {
		return nil
	}

	return killAndWait(ctx, proc, pid, expectArg)
}

// WaitProcessExit blocks until pid exits, the timeout elapses, or ctx ends; a pidfd makes the wait kernel-driven where the OS has one.
func WaitProcessExit(ctx context.Context, pid int, timeout time.Duration) error {
	if handled, err := waitExitWithPidfd(ctx, pid, timeout); handled {
		return err
	}
	return waitDead(ctx, pid, timeout)
}

func killAndWait(ctx context.Context, proc *os.Process, pid int, expectArg string) error {
	match, err := verifyProcessForTermination(pid, expectArg)
	if err != nil || !match {
		return err
	}
	_ = proc.Kill()
	return waitDead(ctx, pid, killWaitTimeout)
}

func waitDead(ctx context.Context, pid int, timeout time.Duration) error {
	return WaitFor(ctx, timeout, time.Millisecond, func() (bool, error) {
		return !IsProcessAlive(pid), nil
	})
}
