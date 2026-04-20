//go:build !linux

package utils

import "time"

func terminateWithPidfd(_ int, _, _ string, _ time.Duration) (bool, error) {
	return false, nil
}
