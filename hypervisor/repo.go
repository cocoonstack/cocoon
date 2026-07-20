package hypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cocoonstack/cocoon/meta"
)

// vmTx is the hypervisor's domain view of one meta transaction, reproducing
// the legacy index idioms (map lookups, explicit name claims, ordered orphan
// dirs) over record-granularity primitives.
type vmTx struct {
	ctx  context.Context
	ns   string
	r    meta.Reader
	w    meta.Writer
	recs *meta.Collection[VMRecord]
	name *meta.Collection[string]
}

// get mirrors idx.VMs[id]: nil when absent.
func (t *vmTx) get(id string) (*VMRecord, error) {
	rec, err := t.recs.Get(t.ctx, t.r, id)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, nil
	}
	return rec, err
}

// getRecord mirrors idx.GetRecord: absence is an error naming the id.
func (t *vmTx) getRecord(id string) (*VMRecord, error) {
	rec, err := t.get(id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("vm %s disappeared from index", id)
	}
	return rec, nil
}

// put mirrors idx.VMs[id] = rec (upsert).
func (t *vmTx) put(id string, rec *VMRecord, opts ...meta.WriteOpt) error {
	err := t.recs.Replace(t.ctx, t.w, id, rec, opts...)
	if errors.Is(err, meta.ErrNotFound) {
		return t.recs.Insert(t.ctx, t.w, id, rec, opts...)
	}
	return err
}

// del mirrors delete(idx.VMs, id).
func (t *vmTx) del(id string) error {
	return t.recs.Delete(t.ctx, t.w, id)
}

// nameGet mirrors idx.Names[name] lookup.
func (t *vmTx) nameGet(name string) (string, bool, error) {
	id, err := t.name.Get(t.ctx, t.r, name)
	if errors.Is(err, meta.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return *id, true, nil
}

// nameSet mirrors idx.Names[name] = id.
func (t *vmTx) nameSet(name, id string, opts ...meta.WriteOpt) error {
	err := t.name.Replace(t.ctx, t.w, name, &id, opts...)
	if errors.Is(err, meta.ErrNotFound) {
		return t.name.Insert(t.ctx, t.w, name, &id, opts...)
	}
	return err
}

// nameDel mirrors delete(idx.Names, name).
func (t *vmTx) nameDel(name string) error {
	return t.name.Delete(t.ctx, t.w, name)
}

// all mirrors reading idx.VMs whole; records are detached.
func (t *vmTx) all() (map[string]*VMRecord, error) {
	return t.recs.List(t.ctx, t.r)
}

func (t *vmTx) scan(fn func(id string, rec *VMRecord) error) error {
	return t.recs.Scan(t.ctx, t.r, fn)
}

// resolve ports utils.ResolveRef: exact ID, then name, then ID prefix >= 3 chars.
func (t *vmTx) resolve(ref string) (string, error) {
	if rec, err := t.get(ref); err != nil {
		return "", err
	} else if rec != nil {
		return ref, nil
	}
	if id, ok, err := t.nameGet(ref); err != nil {
		return "", err
	} else if ok {
		if rec, err := t.get(id); err != nil {
			return "", err
		} else if rec != nil {
			return id, nil
		}
	}
	if len(ref) >= 3 {
		match := ""
		ambiguous := false
		if err := t.r.ScanRaw(t.ctx, t.ns, tableRecords, func(id string, _ json.RawMessage) error {
			if strings.HasPrefix(id, ref) {
				if match != "" {
					ambiguous = true
				}
				match = id
			}
			return nil
		}); err != nil {
			return "", err
		}
		if ambiguous {
			return "", fmt.Errorf("ambiguous ref %q: multiple matches", ref)
		}
		if match != "" {
			return match, nil
		}
	}
	return "", ErrNotFound
}

// resolveMany ports utils.ResolveRefs: batch resolve with dedup.
func (t *vmTx) resolveMany(refs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(refs))
	var ids []string
	for _, ref := range refs {
		id, err := t.resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", ref, err)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
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
		ctx:  ctx,
		ns:   b.NS,
		r:    r,
		w:    w,
		recs: meta.NewCollection[VMRecord](b.Meta, b.NS, tableRecords),
		name: meta.NewCollection[string](b.Meta, b.NS, tableNames),
	}
}
