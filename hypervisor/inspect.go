package hypervisor

import (
	"context"
	"fmt"
	"maps"
	"os"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (b *Backend) Inspect(ctx context.Context, ref string) (*types.VM, error) {
	_, rec, err := b.ResolveAndLoad(ctx, ref)
	if err != nil {
		return nil, err
	}
	return b.ToVM(&rec), nil
}

// List reads every record lock-free (per-record files are atomic-rename generations) and runs ToVM (which does per-running-VM file IO) in parallel, so concurrent writers never queue behind status polls and poll latency stays bounded by pool size, not fleet size.
func (b *Backend) List(ctx context.Context) ([]*types.VM, error) {
	recs, err := b.DB.List()
	if err != nil {
		return nil, err
	}
	return utils.Map(ctx, recs, func(_ context.Context, _ int, r *VMRecord) (*types.VM, error) {
		return b.ToVM(r), nil
	}, b.Conf.EffectivePoolSize())
}

func (b *Backend) ToVM(rec *VMRecord) *types.VM {
	info := rec.VM
	info.Hypervisor = b.Typ
	if info.State == types.VMStateRunning {
		SetRunningSockets(&info, rec.RunDir)
		info.PID, _ = utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
	}
	info.SnapshotIDs = maps.Clone(info.SnapshotIDs)
	return &info
}

func (b *Backend) ResolveRef(_ context.Context, ref string) (string, error) {
	return b.DB.Resolve(ref)
}

// ResolveRefs batch-resolves refs.
func (b *Backend) ResolveRefs(_ context.Context, refs []string) ([]string, error) {
	return b.DB.ResolveMany(refs)
}

// LoadRecord returns a freshly decoded copy private to the caller; still treat it as read-only — mutations belong in DB.Update transactions.
func (b *Backend) LoadRecord(_ context.Context, id string) (VMRecord, error) {
	rec, ok, err := b.DB.Get(id)
	if err != nil {
		return VMRecord{}, err
	}
	if !ok {
		return VMRecord{}, fmt.Errorf("%q not found", id)
	}
	return rec, nil
}

// ResolveAndLoad combines ResolveRef + LoadRecord. A record deleted between
// the two reads maps to ErrNotFound so callers probing backends (resolveVMOwner)
// keep treating it as an absence, not a hard failure.
func (b *Backend) ResolveAndLoad(_ context.Context, ref string) (string, VMRecord, error) {
	id, err := b.DB.Resolve(ref)
	if err != nil {
		return "", VMRecord{}, err
	}
	rec, ok, err := b.DB.Get(id)
	if err != nil {
		return "", VMRecord{}, err
	}
	if !ok {
		return "", VMRecord{}, ErrNotFound
	}
	return id, rec, nil
}

// UpdateRecord runs mutate on the VM's record inside one locked read-modify-write.
func (b *Backend) UpdateRecord(ctx context.Context, vmID string, mutate func(*VMRecord) error) error {
	return b.DB.Update(ctx, vmID, mutate)
}

// SetRunningSockets fills a running VM's live sockets (API socket, bound vsock UDS) from runDir — for clone/restore records that skip ToVM.
func SetRunningSockets(info *types.VM, runDir string) {
	info.SocketPath = SocketPath(runDir)
	if p := VsockSockPath(runDir); isVsockBound(p) {
		info.VsockSocket = p
	}
}

func isVsockBound(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
