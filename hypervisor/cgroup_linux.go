package hypervisor

import (
	"os"
	"os/exec"
	"syscall"
)

// setCmdCgroupFD lands cmd's clone3 directly in the scope (CLONE_INTO_CGROUP), so there is no post-fork attach race.
func setCmdCgroupFD(cmd *exec.Cmd, scope *os.File) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(scope.Fd())
}
