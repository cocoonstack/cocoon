package hypervisor

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// RunningVMClient asserts the VMM process is alive and returns an http.Client on its API socket plus the VM record.
func (b *Backend) RunningVMClient(ctx context.Context, vmRef string) (*http.Client, VMRecord, error) {
	vmID, rec, err := b.ResolveAndLoad(ctx, vmRef)
	if err != nil {
		return nil, VMRecord{}, err
	}
	if err := requireRunning(vmID, &rec); err != nil {
		return nil, VMRecord{}, err
	}
	sockPath := SocketPath(rec.RunDir)
	pid, pidErr := utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
	if pidErr != nil {
		return nil, VMRecord{}, fmt.Errorf("vm %s read pidfile: %w: %w", vmID, pidErr, ErrNotRunning)
	}
	if !utils.VerifyProcessCmdline(pid, sockPath) {
		return nil, VMRecord{}, fmt.Errorf("vm %s pid %d is not its VMM: %w", vmID, pid, ErrNotRunning)
	}
	return utils.NewSocketHTTPClient(sockPath), rec, nil
}

// LockedRunningOp takes the ops lock of a running VM and loads its record under the entry guard; the caller owns unlock.
func (b *Backend) LockedRunningOp(ctx context.Context, vmRef string) (*http.Client, VMRecord, func(), error) {
	hc, live, err := b.RunningVMClient(ctx, vmRef)
	if err != nil {
		return nil, VMRecord{}, nil, err
	}
	vmID := live.ID
	unlock, err := b.LockVMOps(ctx, vmID)
	if err != nil {
		return nil, VMRecord{}, nil, err
	}
	rec, err := b.EntryGuardLoad(ctx, vmID)
	if err == nil {
		err = requireRunning(vmID, &rec)
	}
	if err != nil {
		unlock()
		return nil, VMRecord{}, nil, err
	}
	return hc, rec, unlock, nil
}

func requireRunning(vmID string, rec *VMRecord) error {
	if rec.State == types.VMStateRunning {
		return nil
	}
	return fmt.Errorf("vm %s is %s: %w", vmID, rec.State, ErrNotRunning)
}
