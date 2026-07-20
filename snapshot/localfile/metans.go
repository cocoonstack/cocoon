package localfile

import (
	"context"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/snapshot"
)

const (
	metaNS          = "snapshots"
	tableRecords    = "records"
	tableNames      = "names"
	tableTombstones = "tombstones"
)

// MetaNamespace declares the snapshot namespace over the legacy snapshots.json.
func MetaNamespace(conf *config.Config) metajson.Namespace {
	cfg := NewConfig(conf)
	return metajson.Namespace{
		Name:     metaNS,
		FilePath: cfg.IndexFile(),
		LockPath: cfg.IndexLock(),
		Codec:    snapTables,
	}
}

var snapTables = metajson.TableCodec{Specs: []metajson.TableSpec{
	{Key: "snapshots", Table: tableRecords},
	{Key: "names", Table: tableNames},
	{Key: "tombstones", Table: tableTombstones, Optional: true},
}}

type snapTx = meta.NamedTx[snapshot.SnapshotRecord]

func (lf *LocalFile) view(ctx context.Context, fn func(*snapTx) error) error {
	return lf.meta.View(ctx, []string{metaNS}, func(r meta.Reader) error {
		return fn(lf.tx(ctx, r, nil))
	})
}

func (lf *LocalFile) update(ctx context.Context, fn func(*snapTx) error) error {
	return lf.meta.Update(ctx, meta.Scope{Write: metaNS}, meta.CommitDurable, func(w meta.Writer) error {
		return fn(lf.tx(ctx, w, w))
	})
}

func (lf *LocalFile) tx(ctx context.Context, r meta.Reader, w meta.Writer) *snapTx {
	return meta.NewNamedTx[snapshot.SnapshotRecord](ctx, lf.meta, metaNS, tableRecords, tableNames, r, w)
}
