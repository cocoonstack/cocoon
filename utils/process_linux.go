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

const (
	pfKthread = 0x00200000

	statFlagsField = 6 // index after the ")" that closes comm
)

type aliveFn func(int) bool

type readFileFn func(string) ([]byte, error)

type cmdlineVerifier func(pid int, binaryName, expectArg string) (bool, error)

type procEntry struct {
	pid     int
	cmdline string
}

// ProcScan caches /proc cmdlines for one binaryName.
type ProcScan []procEntry

// ScanProcsByBinary skips processes that vanished mid-scan and fails closed on a live one it cannot read.
func ScanProcsByBinary(binaryName string) (ProcScan, error) {
	return scanProcsByBinary(binaryName, os.ReadFile, IsProcessAlive)
}

// Find returns the cached pids whose cmdline contains expectArg, sorted numerically.
func (s ProcScan) Find(expectArg string) []int {
	var pids []int
	for _, e := range s {
		_, rest, _ := strings.Cut(e.cmdline, "\x00")
		if strings.Contains(rest, expectArg) {
			pids = append(pids, e.pid)
		}
	}
	slices.Sort(pids)
	return pids
}

// FindVMMByCmdline is the one-shot equivalent of ScanProcsByBinary().Find().
func FindVMMByCmdline(binaryName, expectArg string) ([]int, error) {
	scan, err := ScanProcsByBinary(binaryName)
	if err != nil {
		return nil, err
	}
	return scan.Find(expectArg), nil
}

func scanProcsByBinary(binaryName string, readFile readFileFn, alive aliveFn) (ProcScan, error) {
	kthreaddChildren := readKthreaddChildren(readFile)
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
		if _, skip := kthreaddChildren[pid]; skip {
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

// Kernel threads and usermode helpers are the only children, and neither is a VMM.
func readKthreaddChildren(readFile readFileFn) map[int]struct{} {
	stat, err := readFile("/proc/2/stat")
	if err != nil {
		return nil
	}
	_, afterComm, _ := strings.CutLast(string(stat), ")")
	fields := strings.Fields(afterComm)
	if len(fields) <= statFlagsField {
		return nil
	}
	flags, err := strconv.ParseUint(fields[statFlagsField], 10, 64)
	// PID 2 is an ordinary process inside a PID namespace.
	if err != nil || flags&pfKthread == 0 {
		return nil
	}
	children, err := readFile("/proc/2/task/2/children")
	if err != nil {
		return nil
	}
	pids := map[int]struct{}{}
	for field := range strings.FieldsSeq(string(children)) {
		if pid, atoiErr := strconv.Atoi(field); atoiErr == nil {
			pids[pid] = struct{}{}
		}
	}
	return pids
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
	return strings.Contains(rest, expectArg), nil
}

// verifyProcessForTermination fails closed when /proc cannot prove the PID's identity.
func verifyProcessForTermination(pid int, binaryName, expectArg string) (bool, error) {
	return verifyForTermination(pid, binaryName, expectArg, verifyProcessCmdline, IsProcessAlive)
}

func verifyForTermination(pid int, binaryName, expectArg string, verify cmdlineVerifier, alive aliveFn) (bool, error) {
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
