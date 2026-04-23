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

// IsProcessAlive returns true if a process with the given PID currently exists.
// Uses kill(pid, 0) — no signal is sent, only existence is checked.
// EPERM means the process exists but we lack permission to signal it.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// VerifyProcess checks whether pid is running the expected binary.
// On Linux, reads /proc/{pid}/exe. On other platforms, falls back to
// IsProcessAlive (can confirm the process exists but not its binary name).
func VerifyProcess(pid int, binaryName string) bool {
	if pid <= 0 {
		return false
	}
	if match, ok := verifyProcessExe(pid, binaryName); ok {
		return match
	}
	return IsProcessAlive(pid)
}

// VerifyProcessCmdline checks binary name and that expectArg appears in
// /proc/{pid}/cmdline. This prevents cross-instance misidentification when
// multiple processes of the same binary are running (e.g. multiple VMs).
// On non-Linux platforms, falls back to IsProcessAlive.
func VerifyProcessCmdline(pid int, binaryName, expectArg string) bool {
	if pid <= 0 {
		return false
	}
	if expectArg == "" {
		return VerifyProcess(pid, binaryName)
	}
	if match, ok := verifyProcessCmdline(pid, binaryName, expectArg); ok {
		return match
	}
	return IsProcessAlive(pid)
}

// TerminateProcess verifies the PID belongs to binaryName (with optional
// cmdline arg check), then sends SIGTERM, waits up to gracePeriod, and
// falls back to SIGKILL.
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

	if err := WaitFor(ctx, gracePeriod, 100*time.Millisecond, func() (bool, error) { //nolint:mnd
		return !IsProcessAlive(pid), nil
	}); err == nil {
		return nil
	}

	return killAndWait(ctx, proc, pid)
}

func killAndWait(ctx context.Context, proc *os.Process, pid int) error {
	_ = proc.Kill()
	return WaitFor(ctx, killWaitTimeout, 50*time.Millisecond, func() (bool, error) { //nolint:mnd
		return !IsProcessAlive(pid), nil
	})
}
