//go:build !linux

package hypervisor

import (
	"os"
	"os/exec"
)

// setCmdCgroupFD is Linux-only (CLONE_INTO_CGROUP); other platforms never launch VMMs.
func setCmdCgroupFD(*exec.Cmd, *os.File) {}
