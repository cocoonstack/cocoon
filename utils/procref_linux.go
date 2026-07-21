//go:build linux

package utils

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// startTimeField is starttime's 0-based index among the stat fields that follow comm's closing paren.
const startTimeField = 19

// procStartTime reads pid's start time from /proc/<pid>/stat. Parsing resumes
// after the last ')' because comm is unquoted and may itself contain parens.
func procStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) //nolint:gosec // internal runtime path
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, fmt.Errorf("parse /proc/%d/stat: no comm terminator", pid)
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) <= startTimeField {
		return 0, fmt.Errorf("parse /proc/%d/stat: got %d fields after comm", pid, len(fields))
	}
	start, err := strconv.ParseUint(fields[startTimeField], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/%d/stat starttime: %w", pid, err)
	}
	return start, nil
}
