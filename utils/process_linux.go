//go:build linux

package utils

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type procEntry struct {
	pid     int
	cmdline string
}

// ProcScan caches /proc cmdlines for one binaryName. Batch callers scan once then Find per id, replacing N /proc walks with one.
type ProcScan []procEntry

// ScanProcsByBinary walks /proc once, capturing argv[0]-basename matches. ENOENT (process exited mid-scan) is skipped; other read errors fail closed.
func ScanProcsByBinary(binaryName string) (ProcScan, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var scan ProcScan
	var firstErr error
	for _, e := range entries {
		pid, atoiErr := strconv.Atoi(e.Name())
		if atoiErr != nil || pid <= 0 {
			continue
		}
		data, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if readErr != nil {
			if !errors.Is(readErr, fs.ErrNotExist) && firstErr == nil {
				firstErr = fmt.Errorf("read /proc/%d/cmdline: %w", pid, readErr)
			}
			continue
		}
		argv0, _, _ := strings.Cut(string(data), "\x00")
		if filepath.Base(argv0) != binaryName {
			continue
		}
		scan = append(scan, procEntry{pid: pid, cmdline: string(data)})
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return scan, nil
}

// Find returns the cached pids whose cmdline contains expectArg, sorted numerically; empty expectArg matches all.
func (s ProcScan) Find(expectArg string) []int {
	var pids []int
	for _, e := range s {
		_, rest, _ := strings.Cut(e.cmdline, "\x00")
		if expectArg == "" || strings.Contains(rest, expectArg) {
			pids = append(pids, e.pid)
		}
	}
	slices.Sort(pids)
	return pids
}

// FindVMMByCmdline is the one-shot equivalent of ScanProcsByBinary().Find(); batch callers should use ScanProcsByBinary directly to share one /proc walk.
func FindVMMByCmdline(binaryName, expectArg string) ([]int, error) {
	scan, err := ScanProcsByBinary(binaryName)
	if err != nil {
		return nil, err
	}
	return scan.Find(expectArg), nil
}

// Match argv[0] basename strictly + expectArg substring so "bash -c 'cloud-hypervisor ...'" can't impersonate the VMM.
func verifyProcessCmdline(pid int, binaryName, expectArg string) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false, err
	}
	argv0, rest, _ := strings.Cut(string(data), "\x00")
	if filepath.Base(argv0) != binaryName {
		return false, nil
	}
	if expectArg == "" {
		return true, nil
	}
	return strings.Contains(rest, expectArg), nil
}
