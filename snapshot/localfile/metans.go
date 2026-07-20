package localfile

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/snapshot"
)

const (
	metaNS     = "snapshots"
	tableRecs  = "records"
	tableNames = "names"
)

// MetaNamespace declares the snapshot namespace over the legacy snapshots.json.
func MetaNamespace(conf *config.Config) metajson.Namespace {
	cfg := NewConfig(conf)
	return metajson.Namespace{
		Name:     metaNS,
		FilePath: cfg.IndexFile(),
		LockPath: cfg.IndexLock(),
		Codec:    snapIndexCodec{},
	}
}

var _ metajson.Codec = snapIndexCodec{}

// snapIndexCodec reproduces the legacy SnapshotIndex file byte-for-byte.
type snapIndexCodec struct{}

func (snapIndexCodec) Decode(data []byte) (*metajson.Model, error) {
	m := metajson.NewModel()
	if data == nil {
		return m, nil
	}
	var idx snapshot.SnapshotIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(idx.Snapshots)) {
		raw, err := json.Marshal(idx.Snapshots[id])
		if err != nil {
			return nil, err
		}
		m.Put(tableRecs, id, raw)
	}
	for _, name := range slices.Sorted(maps.Keys(idx.Names)) {
		raw, err := json.Marshal(idx.Names[name])
		if err != nil {
			return nil, err
		}
		m.Put(tableNames, name, raw)
	}
	return m, nil
}

func (snapIndexCodec) Encode(m *metajson.Model) ([]byte, error) {
	idx := snapshot.SnapshotIndex{}
	idx.Init()
	if err := m.Scan(tableRecs, func(id string, raw json.RawMessage) error {
		var rec snapshot.SnapshotRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return err
		}
		idx.Snapshots[id] = &rec
		return nil
	}); err != nil {
		return nil, err
	}
	if err := m.Scan(tableNames, func(name string, raw json.RawMessage) error {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return err
		}
		idx.Names[name] = id
		return nil
	}); err != nil {
		return nil, err
	}
	data, err := json.Marshal(&idx)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

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

// rawView reads while the GC orchestrator holds the namespace lock (legacy ReadRaw).
func (lf *LocalFile) rawView(ctx context.Context, fn func(*snapTx) error) error {
	return lf.meta.RawView(ctx, metaNS, func(r meta.Reader) error {
		return fn(lf.tx(ctx, r, nil))
	})
}

// lockedUpdate writes while the GC orchestrator holds the namespace lock (legacy WriteRaw).
func (lf *LocalFile) lockedUpdate(ctx context.Context, fn func(*snapTx) error) error {
	return lf.meta.LockedUpdate(ctx, metaNS, func(w meta.Writer) error {
		return fn(lf.tx(ctx, w, w))
	})
}

func (lf *LocalFile) tx(ctx context.Context, r meta.Reader, w meta.Writer) *snapTx {
	return meta.NewNamedTx[snapshot.SnapshotRecord](ctx, lf.meta, metaNS, tableRecs, tableNames, r, w)
}
