//go:build linux

package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// aliveFn reports whether a pid is still running.
type aliveFn func(int) bool

type procEntry struct {
	pid     int
	cmdline string
}

// ProcScan caches /proc cmdlines for one binaryName. Batch callers scan once then Find per id, replacing N /proc walks with one.
type ProcScan []procEntry

// ScanProcsByBinary walks /proc once, capturing argv[0]-basename matches. Read errors from processes that vanished mid-scan are skipped; errors on live processes fail closed.
func ScanProcsByBinary(binaryName string) (ProcScan, error) {
	return scanProcsByBinary(binaryName, os.ReadFile, IsProcessAlive)
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

func scanProcsByBinary(binaryName string, readFile func(string) ([]byte, error), alive aliveFn) (ProcScan, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var scan ProcScan
	for _, e := range entries {
		pid, atoiErr := strconv.Atoi(e.Name())
		if atoiErr != nil || pid <= 0 {
			continue
		}
		data, readErr := readFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if readErr != nil {
			if alive(pid) {
				return nil, readErr
			}
			continue
		}
		argv0, _, _ := strings.Cut(string(data), "\x00")
		if filepath.Base(argv0) != binaryName {
			continue
		}
		scan = append(scan, procEntry{pid: pid, cmdline: string(data)})
	}
	return scan, nil
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

// verifyProcessForTermination fails closed when /proc cannot prove the PID's identity.
func verifyProcessForTermination(pid int, binaryName, expectArg string) (bool, error) {
	return verifyForTermination(pid, binaryName, expectArg, verifyProcessCmdline, IsProcessAlive)
}

func verifyForTermination(pid int, binaryName, expectArg string, verify func(int, string, string) (bool, error), alive aliveFn) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	match, err := verify(pid, binaryName, expectArg)
	if err == nil {
		return match, nil
	}
	if !alive(pid) {
		return false, nil
	}
	return false, fmt.Errorf("verify process %d identity: %w", pid, err)
}
