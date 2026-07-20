package hypervisor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cocoonstack/cocoon/meta"
)

// vmTx is the hypervisor's domain view of one meta transaction: the shared
// named-index pattern plus the ordered orphan-dir intent list.
type vmTx struct {
	*meta.NamedTx[VMRecord]

	ctx context.Context
	ns  string
	r   meta.Reader
	w   meta.Writer
}

// getRecord mirrors idx.GetRecord: absence is an error naming the id.
func (t *vmTx) getRecord(id string) (*VMRecord, error) {
	rec, err := t.Get(id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("vm %s disappeared from index", id)
	}
	return rec, nil
}

func (t *vmTx) resolve(ref string) (string, error) {
	return t.Resolve(ref, ErrNotFound)
}

func (t *vmTx) resolveMany(refs []string) ([]string, error) {
	return t.ResolveMany(refs, ErrNotFound)
}

// orphanDirs returns the ordered cleanup-intent list.
func (t *vmTx) orphanDirs() ([]string, error) {
	var dirs []string
	if err := t.r.ScanRaw(t.ctx, t.ns, tableOrphanDirs, func(dir string, _ json.RawMessage) error {
		dirs = append(dirs, dir)
		return nil
	}); err != nil {
		return nil, err
	}
	return dirs, nil
}

// addOrphanDir mirrors the contains-check append on idx.OrphanDirs.
func (t *vmTx) addOrphanDir(dir string) error {
	if _, ok, err := t.w.GetRaw(t.ctx, t.ns, tableOrphanDirs, dir); err != nil || ok {
		return err
	}
	return t.w.PutRaw(t.ctx, t.ns, tableOrphanDirs, dir, json.RawMessage(orphanDirEntry), false)
}

// removeOrphanDir mirrors slices.DeleteFunc on idx.OrphanDirs.
func (t *vmTx) removeOrphanDir(dir string) error {
	return t.w.DeleteRaw(t.ctx, t.ns, tableOrphanDirs, dir, false)
}

// view runs fn over the backend's namespace snapshot (legacy With).
func (b *Backend) view(ctx context.Context, fn func(*vmTx) error) error {
	return b.Meta.View(ctx, []string{b.NS}, func(r meta.Reader) error {
		return fn(b.tx(ctx, r, nil))
	})
}

// update runs fn in a durable transaction (legacy Update).
func (b *Backend) update(ctx context.Context, fn func(*vmTx) error) error {
	return b.Meta.Update(ctx, meta.Scope{Write: b.NS}, meta.CommitDurable, func(w meta.Writer) error {
		return fn(b.tx(ctx, w, w))
	})
}

// updateRelaxed is the creating-placeholder write, today's only relaxed flow
// (legacy UpdateNoDirSync): its loss is re-derived by the GC orphan sweep.
func (b *Backend) updateRelaxed(ctx context.Context, fn func(*vmTx) error) error {
	return b.Meta.Update(ctx, meta.Scope{Write: b.NS}, meta.CommitRelaxed, func(w meta.Writer) error {
		return fn(b.tx(ctx, w, w))
	})
}

// rawView is the lockless read (legacy ReadRaw); P0 allowlist: LockVMOps and
// the GC snapshot paths that already hold the namespace lock.
func (b *Backend) rawView(ctx context.Context, fn func(*vmTx) error) error {
	return b.Meta.RawView(ctx, b.NS, func(r meta.Reader) error {
		return fn(b.tx(ctx, r, nil))
	})
}

// lockedUpdate writes while the GC orchestrator holds the namespace lock (legacy WriteRaw).
func (b *Backend) lockedUpdate(ctx context.Context, fn func(*vmTx) error) error {
	return b.Meta.LockedUpdate(ctx, b.NS, func(w meta.Writer) error {
		return fn(b.tx(ctx, w, w))
	})
}

func (b *Backend) tx(ctx context.Context, r meta.Reader, w meta.Writer) *vmTx {
	return &vmTx{
		NamedTx: meta.NewNamedTx[VMRecord](ctx, b.Meta, b.NS, tableRecords, tableNames, r, w),
		ctx:     ctx,
		ns:      b.NS,
		r:       r,
		w:       w,
	}
}
