//go:build !linux

package utils

import "fmt"

// procStartTime has no portable source outside Linux; identity degrades to pid-only.
func procStartTime(pid int) (uint64, error) {
	if !IsProcessAlive(pid) {
		return 0, fmt.Errorf("process %d is not alive", pid)
	}
	return 0, nil
}
