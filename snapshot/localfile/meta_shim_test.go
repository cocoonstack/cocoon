package localfile

import (
	"context"
	"maps"

	"github.com/cocoonstack/cocoon/snapshot"
)

// dbUpdate is the test-only whole-index shim: materialize, run fn, write the
// difference back. Production code never uses it.
func (lf *LocalFile) dbUpdate(ctx context.Context, fn func(*snapshot.SnapshotIndex) error) error {
	return lf.update(ctx, func(t *snapTx) error {
		before, idx, err := materializeSnapIndex(t)
		if err != nil {
			return err
		}
		if err := fn(idx); err != nil {
			return err
		}
		return writeBackSnapIndex(t, before, idx)
	})
}

// dbRead is the test-only whole-index read shim.
func (lf *LocalFile) dbRead(ctx context.Context, fn func(*snapshot.SnapshotIndex) error) error {
	return lf.view(ctx, func(t *snapTx) error {
		_, idx, err := materializeSnapIndex(t)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

func materializeSnapIndex(t *snapTx) (*snapshot.SnapshotIndex, *snapshot.SnapshotIndex, error) {
	idx := &snapshot.SnapshotIndex{}
	idx.Init()
	var err error
	if idx.Snapshots, err = t.All(); err != nil {
		return nil, nil, err
	}
	if err := t.Scan(func(string, *snapshot.SnapshotRecord) error { return nil }); err != nil {
		return nil, nil, err
	}
	for _, rec := range idx.Snapshots {
		if rec != nil && rec.Name != "" {
			if id, ok, err := t.NameGet(rec.Name); err != nil {
				return nil, nil, err
			} else if ok {
				idx.Names[rec.Name] = id
			}
		}
	}
	before := &snapshot.SnapshotIndex{Snapshots: maps.Clone(idx.Snapshots), Names: maps.Clone(idx.Names)}
	return before, idx, nil
}

func writeBackSnapIndex(t *snapTx, before, after *snapshot.SnapshotIndex) error {
	for id := range before.Snapshots {
		if after.Snapshots[id] == nil {
			if err := t.Del(id); err != nil {
				return err
			}
		}
	}
	for id, rec := range after.Snapshots {
		if rec == nil {
			continue
		}
		if err := t.Put(id, rec); err != nil {
			return err
		}
	}
	for name := range before.Names {
		if _, ok := after.Names[name]; !ok {
			if err := t.NameDel(name); err != nil {
				return err
			}
		}
	}
	for name, id := range after.Names {
		if err := t.NameSet(name, id); err != nil {
			return err
		}
	}
	return nil
}
