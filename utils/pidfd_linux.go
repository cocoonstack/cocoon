//go:build linux

package utils

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// OpenPidfd returns a pidfd for pid; the fd becomes readable once the process exits.
// pidfdPollSlice bounds one poll so ctx cancellation is honored.
const pidfdPollSlice = 100 * time.Millisecond

func OpenPidfd(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }

// CloseFD closes a raw descriptor, ignoring a zero or negative one.
func CloseFD(fd int) {
	if fd > 0 {
		_ = unix.Close(fd)
	}
}

// terminateWithPidfd uses pidfd_open + pidfd_send_signal for TOCTOU-safe process termination. Returns false if pidfd is unavailable (kernel < 5.3).
func terminateWithPidfd(ctx context.Context, pid int, binaryName, expectArg string, gracePeriod time.Duration) (handled bool, err error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false, nil
	}
	defer func() { _ = unix.Close(fd) }()

	match, err := verifyProcessForTermination(pid, binaryName, expectArg)
	if err != nil {
		return true, err
	}
	if !match {
		return true, nil
	}

	if err := unix.PidfdSendSignal(fd, syscall.SIGTERM, nil, 0); err != nil {
		if !IsProcessAlive(pid) {
			return true, nil
		}
		_ = unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0)
		return true, waitPidfd(ctx, fd, killWaitTimeout)
	}

	if err := waitPidfd(ctx, fd, gracePeriod); err == nil {
		return true, nil
	}

	if err := unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0); err != nil {
		if !IsProcessAlive(pid) {
			return true, nil
		}
	}
	return true, waitPidfd(ctx, fd, killWaitTimeout)
}

// waitPidfd blocks until the pidfd reports exit; the kernel wakes the poll instead of a kill(0) loop.
func waitPidfd(ctx context.Context, fd int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timeout after %s", timeout)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // a pidfd is a small non-negative descriptor
		n, err := unix.Poll(fds, int(min(remaining, pidfdPollSlice).Milliseconds()))
		if err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("poll pidfd: %w", err)
		}
		if n > 0 {
			return nil
		}
	}
}
