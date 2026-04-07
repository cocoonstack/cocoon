package firecracker

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	apiSockName = "api.sock"
	pidFileName = "fc.pid"
)

func toVM(rec *hypervisor.VMRecord) *types.VM {
	info := rec.VM // value copy — detached from the DB record
	if info.State == types.VMStateRunning {
		info.SocketPath = socketPath(rec.RunDir)
		info.PID, _ = utils.ReadPIDFile(pidFile(rec.RunDir))
	}
	return &info
}

// socketPath returns the API socket path under a VM's run directory.
func socketPath(runDir string) string { return filepath.Join(runDir, apiSockName) }

// pidFile returns the PID file path under a VM's run directory.
func pidFile(runDir string) string { return filepath.Join(runDir, pidFileName) }
