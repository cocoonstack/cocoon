//go:build !linux

package utils

import (
	"context"
	"errors"
	"time"
)

var errPidfdUnsupported = errors.New("pidfd is unsupported on this OS")

// OpenPidfd has no counterpart outside Linux.
func OpenPidfd(int) (int, error) { return 0, errPidfdUnsupported }

// CloseFD is a no-op where no descriptor is ever opened.
func CloseFD(int) {}

func terminateWithPidfd(_ context.Context, _ int, _, _ string, _ time.Duration) (bool, error) {
	return false, nil
}
