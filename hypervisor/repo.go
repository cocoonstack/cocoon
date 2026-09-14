package hypervisor

import (
	"cmp"
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/types"
)

type vmTxFunc func(*vmTx) error

type vmTx struct {
	*meta.NamedTx[VMRecord]

	ctx        context.Context
	r          meta.Reader
	w          meta.Writer
	placements *meta.Collection[string]
}

// Put writes rec and mirrors its placement row, which the launch tally scans instead of every record.
func (t *vmTx) Put(id string, rec *VMRecord, opts ...meta.WriteOpt) error {
	if err := t.NamedTx.Put(id, rec, opts...); err != nil {
		return err
	}
	if cpus := cmp.Or(rec.QueueCPUs, rec.CPUSet); cpus != "" {
		return t.placements.Upsert(t.ctx, t.w, id, &cpus, opts...)
	}
	return t.placements.Delete(t.ctx, t.w, id, opts...)
}

func (t *vmTx) Del(id string) error {
	if err := t.NamedTx.Del(id); err != nil {
		return err
	}
	return t.placements.Delete(t.ctx, t.w, id)
}

func (t *vmTx) loadDetached(id string) (VMRecord, error) {
	rec, err := t.getRecord(id)
	if err != nil {
		return VMRecord{}, err
	}
	return *rec, nil
}

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

func (b *Backend) view(ctx context.Context, fn vmTxFunc) error {
	return b.Meta.View(ctx, []string{b.NS}, func(r meta.Reader) error {
		return fn(b.tx(ctx, r, nil))
	})
}

func (b *Backend) update(ctx context.Context, fn vmTxFunc) error {
	return b.Meta.Update(ctx, meta.Scope{Write: b.NS}, meta.CommitDurable, func(w meta.Writer) error {
		return fn(b.tx(ctx, w, w))
	})
}

// updateRelaxed skips the durable commit; every caller's write is re-derived by a later pass.
func (b *Backend) updateRelaxed(ctx context.Context, read []string, fn vmTxFunc) error {
	return b.Meta.Update(ctx, meta.Scope{Write: b.NS, Read: read}, meta.CommitRelaxed, func(w meta.Writer) error {
		return fn(b.tx(ctx, w, w))
	})
}

func (b *Backend) tx(ctx context.Context, r meta.Reader, w meta.Writer) *vmTx {
	return &vmTx{
		NamedTx:    meta.NewNamedTx[VMRecord](ctx, b.NS, TableRecords, TableNames, r, w),
		ctx:        ctx,
		r:          r,
		w:          w,
		placements: meta.NewCollection[string](b.NS, TablePlacements),
	}
}

func validateRecordInvariants(rec *VMRecord) error {
	if err := types.ValidateStorageConfigs(rec.StorageConfigs); err != nil {
		return fmt.Errorf("storage invariants violated: %w", err)
	}
	if err := types.ValidateNetworkConfigs(rec.NetworkConfigs); err != nil {
		return fmt.Errorf("network invariants violated: %w", err)
	}
	return nil
}
