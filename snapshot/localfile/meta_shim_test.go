package localfile

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/snapshot"
)

func TestLegacyDifferentialTrace(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	baseline, err := os.ReadFile("testdata/legacy-snapshots.baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile("testdata/legacy-snapshots.after.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "snapshots.json")
	if err := os.WriteFile(path, baseline, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := metajson.Open(metajson.Namespace{
		Name: NamespaceName, FilePath: path, LockPath: filepath.Join(dir, "snapshots.lock"), Codec: testSnapTables,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lf := &LocalFile{meta: store}

	if err := lf.update(ctx, func(*snapTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(baseline) {
		t.Fatalf("round-trip not byte-identical:\n got: %s\nwant: %s", got, baseline)
	}

	t2 := time.Date(2026, 7, 3, 12, 45, 0, 0, time.UTC)
	if err := lf.update(ctx, func(tx *snapTx) error {
		p, err := tx.Get("SNAP333333333333")
		if err != nil {
			return err
		}
		p.Pending = false
		p.SizeBytes = 8192
		p.LastAccessedAt = t2
		if err := tx.Put("SNAP333333333333", p); err != nil {
			return err
		}
		n, err := tx.Get("SNAP111111111111")
		if err != nil {
			return err
		}
		n.LastAccessedAt = t2
		if err := tx.Put("SNAP111111111111", n); err != nil {
			return err
		}
		return tx.Del("SNAP222222222222")
	}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(after) {
		t.Fatalf("differential trace diverged:\n got: %s\nwant: %s", got, after)
	}
}

type snapshotIndex struct {
	Snapshots map[string]*snapshot.SnapshotRecord `json:"snapshots"`
	Names     map[string]string                   `json:"names"`
}

func (idx *snapshotIndex) Init() {
	if idx.Snapshots == nil {
		idx.Snapshots = map[string]*snapshot.SnapshotRecord{}
	}
	if idx.Names == nil {
		idx.Names = map[string]string{}
	}
}

func (lf *LocalFile) dbUpdate(ctx context.Context, fn func(*snapshotIndex) error) error {
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

func (lf *LocalFile) dbRead(ctx context.Context, fn func(*snapshotIndex) error) error {
	return lf.view(ctx, func(t *snapTx) error {
		_, idx, err := materializeSnapIndex(t)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

func materializeSnapIndex(t *snapTx) (*snapshotIndex, *snapshotIndex, error) {
	idx := &snapshotIndex{}
	idx.Init()
	var err error
	if idx.Snapshots, err = t.All(); err != nil {
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
	before := &snapshotIndex{Snapshots: maps.Clone(idx.Snapshots), Names: maps.Clone(idx.Names)}
	return before, idx, nil
}

func writeBackSnapIndex(t *snapTx, before, after *snapshotIndex) error {
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
