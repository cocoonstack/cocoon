package localfile

import (
	"context"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/snapshot"
)

const (
	// NamespaceName and the table names are the snapshots data-model
	// vocabulary; the composition root maps them onto the legacy file.
	NamespaceName = "snapshots"
	TableRecords  = "records"
	TableNames    = "names"
)

type snapTx = meta.NamedTx[snapshot.SnapshotRecord]

func (lf *LocalFile) view(ctx context.Context, fn func(*snapTx) error) error {
	return lf.meta.View(ctx, []string{NamespaceName}, func(r meta.Reader) error {
		return fn(lf.tx(ctx, r, nil))
	})
}

func (lf *LocalFile) update(ctx context.Context, fn func(*snapTx) error) error {
	return lf.meta.Update(ctx, meta.Scope{Write: NamespaceName}, meta.CommitDurable, func(w meta.Writer) error {
		return fn(lf.tx(ctx, w, w))
	})
}

func (lf *LocalFile) tx(ctx context.Context, r meta.Reader, w meta.Writer) *snapTx {
	return meta.NewNamedTx[snapshot.SnapshotRecord](ctx, lf.meta, NamespaceName, TableRecords, TableNames, r, w)
}
