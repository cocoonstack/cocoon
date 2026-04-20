//go:build !linux

package utils

import (
	"context"
	"time"
)

func terminateWithPidfd(_ context.Context, _ int, _, _ string, _ time.Duration) (bool, error) {
	return false, nil
}
