package hypervisor

import (
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon/meta"
)

type vmTx struct {
	*meta.NamedTx[VMRecord]

	ctx context.Context
	ns  string
	r   meta.Reader
	w   meta.Writer
}

func (t *vmTx) loadDetached(id string) (VMRecord, error) {
	r, err := t.Get(id)
	if err != nil {
		return VMRecord{}, err
	}
	if r == nil {
		return VMRecord{}, fmt.Errorf("%q not found", id)
	}
	return *r, nil
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

func (b *Backend) view(ctx context.Context, fn func(*vmTx) error) error {
	return b.Meta.View(ctx, []string{b.NS}, func(r meta.Reader) error {
		return fn(b.tx(ctx, r, nil))
	})
}

func (b *Backend) update(ctx context.Context, fn func(*vmTx) error) error {
	return b.Meta.Update(ctx, meta.Scope{Write: b.NS}, meta.CommitDurable, func(w meta.Writer) error {
		return fn(b.tx(ctx, w, w))
	})
}

// updateRelaxed is the creating-placeholder write, today's only relaxed flow: its loss is re-derived by the GC orphan sweep.
func (b *Backend) updateRelaxed(ctx context.Context, fn func(*vmTx) error) error {
	return b.Meta.Update(ctx, meta.Scope{Write: b.NS}, meta.CommitRelaxed, func(w meta.Writer) error {
		return fn(b.tx(ctx, w, w))
	})
}

func (b *Backend) tx(ctx context.Context, r meta.Reader, w meta.Writer) *vmTx {
	return &vmTx{
		NamedTx: meta.NewNamedTx[VMRecord](ctx, b.NS, TableRecords, TableNames, r, w),
		ctx:     ctx,
		ns:      b.NS,
		r:       r,
		w:       w,
	}
}
