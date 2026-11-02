//go:build !linux

package utils

import "errors"

type ProcScan struct{}

func ScanProcsByBinary(_ string) (ProcScan, error) { return ProcScan{}, nil }

func (ProcScan) Find(_ string) []int { return nil }

func FindVMMByCmdline(_, _ string) ([]int, error) {
	return nil, nil
}

func verifyProcessCmdline(_ int, _, _ string) (bool, error) {
	return false, errors.New("verifyProcessCmdline: unsupported on this OS")
}

func verifyProcessForTermination(pid int, binaryName, expectArg string) (bool, error) {
	return VerifyProcessCmdline(pid, binaryName, expectArg), nil
}
