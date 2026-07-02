package hypervisor

import (
	"context"
	"maps"
	"os"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (b *Backend) Inspect(ctx context.Context, ref string) (*types.VM, error) {
	var result *types.VM
	return result, b.DB.With(ctx, func(idx *VMIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		result = b.ToVM(idx.VMs[id])
		return nil
	})
}

// List snapshots all records under the DB lock then runs ToVM (which does file IO) outside the lock so concurrent writers don't queue behind status polls. Mutable map fields are cloned inside the lock to avoid a concurrent-read race with RecordSnapshot etc.
func (b *Backend) List(ctx context.Context) ([]*types.VM, error) {
	var recs []*VMRecord
	if err := b.DB.With(ctx, func(idx *VMIndex) error {
		recs = utils.MapValues(idx.VMs, func(r *VMRecord) *VMRecord {
			cp := *r
			cp.SnapshotIDs = maps.Clone(r.SnapshotIDs)
			return &cp
		})
		return nil
	}); err != nil {
		return nil, err
	}
	result := make([]*types.VM, len(recs))
	for i, r := range recs {
		result[i] = b.ToVM(r)
	}
	return result, nil
}

func (b *Backend) ToVM(rec *VMRecord) *types.VM {
	info := rec.VM
	info.Hypervisor = b.Typ
	if info.State == types.VMStateRunning {
		info.SocketPath = SocketPath(rec.RunDir)
		info.PID, _ = utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
		// Empty for legacy VMs whose UDS isn't bound.
		if p := VsockSockPath(rec.RunDir); isVsockBound(p) {
			info.VsockSocket = p
		}
	}
	info.SnapshotIDs = maps.Clone(info.SnapshotIDs)
	return &info
}

func (b *Backend) ResolveRef(ctx context.Context, ref string) (string, error) {
	var id string
	return id, b.DB.With(ctx, func(idx *VMIndex) error {
		var err error
		id, err = idx.Resolve(ref)
		return err
	})
}

// ResolveRefs batch-resolves under a single lock.
func (b *Backend) ResolveRefs(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	return ids, b.DB.With(ctx, func(idx *VMIndex) error {
		var err error
		ids, err = idx.ResolveMany(refs)
		return err
	})
}

// LoadRecord returns a shallow value-copy; pointer/slice/map fields still alias the live record. Treat as read-only outside DB transactions.
func (b *Backend) LoadRecord(ctx context.Context, id string) (VMRecord, error) {
	var rec VMRecord
	return rec, b.DB.With(ctx, func(idx *VMIndex) error {
		var err error
		rec, err = utils.LookupCopy(idx.VMs, id)
		return err
	})
}

// ResolveAndLoad combines ResolveRef + LoadRecord under a single DB lock.
func (b *Backend) ResolveAndLoad(ctx context.Context, ref string) (string, VMRecord, error) {
	var (
		id  string
		rec VMRecord
	)
	return id, rec, b.DB.With(ctx, func(idx *VMIndex) error {
		var err error
		id, err = idx.Resolve(ref)
		if err != nil {
			return err
		}
		rec, err = utils.LookupCopy(idx.VMs, id)
		return err
	})
}

func isVsockBound(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
