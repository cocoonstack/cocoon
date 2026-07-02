//go:build linux

package utils

import (
	"os"
	"strconv"
	"strings"
)

// DetectHugePages returns true iff /proc/sys/vm/nr_hugepages > 0; false on any read/parse error.
func DetectHugePages() bool {
	data, err := os.ReadFile("/proc/sys/vm/nr_hugepages")
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return err == nil && n > 0
}
