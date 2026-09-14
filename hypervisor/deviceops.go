package hypervisor

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// RunningVMClient asserts the VMM process is alive and returns an http.Client on its API socket plus the VM id.
func (b *Backend) RunningVMClient(ctx context.Context, vmRef string) (*http.Client, string, error) {
	vmID, rec, err := b.ResolveAndLoad(ctx, vmRef)
	if err != nil {
		return nil, "", err
	}
	if rec.State != types.VMStateRunning {
		return nil, "", fmt.Errorf("vm %s is %s: %w", vmID, rec.State, ErrNotRunning)
	}
	sockPath := SocketPath(rec.RunDir)
	pid, pidErr := utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
	if pidErr != nil {
		return nil, "", fmt.Errorf("vm %s read pidfile: %w: %w", vmID, pidErr, ErrNotRunning)
	}
	if !utils.VerifyProcessCmdline(pid, b.Conf.BinaryName(), sockPath) {
		return nil, "", fmt.Errorf("vm %s pid %d not %s: %w", vmID, pid, b.Conf.BinaryName(), ErrNotRunning)
	}
	return utils.NewSocketHTTPClient(sockPath), vmID, nil
}

// LockedRunningOp takes the ops lock of a running VM and loads its record under the entry guard; the caller owns unlock.
func (b *Backend) LockedRunningOp(ctx context.Context, vmRef string) (*http.Client, VMRecord, func(), error) {
	hc, vmID, err := b.RunningVMClient(ctx, vmRef)
	if err != nil {
		return nil, VMRecord{}, nil, err
	}
	unlock, err := b.LockVMOps(ctx, vmID)
	if err != nil {
		return nil, VMRecord{}, nil, err
	}
	rec, err := b.EntryGuardLoad(ctx, vmID)
	if err != nil {
		unlock()
		return nil, VMRecord{}, nil, err
	}
	return hc, rec, unlock, nil
}
