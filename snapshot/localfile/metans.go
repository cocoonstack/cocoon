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
		Codec:    snapIndexCodec{},
	}
}

// rawSnapIndex mirrors SnapshotIndex's field layout with pass-through record bytes.
type rawSnapIndex struct {
	Snapshots  map[string]json.RawMessage `json:"snapshots"`
	Names      map[string]json.RawMessage `json:"names"`
	Tombstones map[string]json.RawMessage `json:"tombstones,omitempty"`
}

var _ metajson.Codec = snapIndexCodec{}

// snapIndexCodec reproduces the legacy SnapshotIndex file byte-for-byte;
// records cross as raw messages (no per-record re-marshal).
type snapIndexCodec struct{}

func (snapIndexCodec) Decode(data []byte) (*metajson.Model, error) {
	m := metajson.NewModel()
	if data == nil {
		return m, nil
	}
	var idx rawSnapIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(idx.Snapshots)) {
		m.Put(tableRecords, id, idx.Snapshots[id])
	}
	for _, name := range slices.Sorted(maps.Keys(idx.Names)) {
		m.Put(tableNames, name, idx.Names[name])
	}
	for _, id := range slices.Sorted(maps.Keys(idx.Tombstones)) {
		m.Put(tableTombstones, id, idx.Tombstones[id])
	}
	return m, nil
}

func (snapIndexCodec) Encode(m *metajson.Model) ([]byte, error) {
	buf := append([]byte(nil), `{"snapshots":`...)
	buf, err := metajson.AppendRawMap(buf, metajson.CollectTable(m, tableRecords))
	if err != nil {
		return nil, err
	}
	buf = append(buf, `,"names":`...)
	if buf, err = metajson.AppendRawMap(buf, metajson.CollectTable(m, tableNames)); err != nil {
		return nil, err
	}
	if ts := metajson.CollectTable(m, tableTombstones); len(ts) > 0 {
		buf = append(buf, `,"tombstones":`...)
		if buf, err = metajson.AppendRawMap(buf, ts); err != nil {
			return nil, err
		}
	}
	return append(buf, '}', '\n'), nil
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

func (lf *LocalFile) tx(ctx context.Context, r meta.Reader, w meta.Writer) *snapTx {
	return meta.NewNamedTx[snapshot.SnapshotRecord](ctx, lf.meta, metaNS, tableRecords, tableNames, r, w)
}
