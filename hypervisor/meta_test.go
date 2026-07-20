package hypervisor

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"testing"

	metajson "github.com/cocoonstack/cocoon/meta/json"
)

// newTestMetaStore opens a meta store over conf's index paths for one backend type.
func newTestMetaStore(t *testing.T, typ string, conf BackendConfig) *metajson.Store {
	t.Helper()
	store, err := metajson.Open(MetaNamespace(typ, conf))
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// testNamespace builds a standalone namespace under dir for shim-level tests.
func testNamespace(t *testing.T, typ, dir string) *metajson.Store {
	t.Helper()
	store, err := metajson.Open(metajson.Namespace{
		Name:     VMNamespaceName(typ),
		FilePath: filepath.Join(dir, "index.json"),
		LockPath: filepath.Join(dir, "index.lock"),
		Codec:    vmIndexCodec{},
	})
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// dbUpdate is the test-only whole-index shim: materialize, run fn, write the
// difference back. Production code never uses it.
func (b *Backend) dbUpdate(ctx context.Context, fn func(*VMIndex) error) error {
	return b.update(ctx, func(t *vmTx) error {
		before, idx, err := materialize(t)
		if err != nil {
			return err
		}
		if err := fn(idx); err != nil {
			return err
		}
		return writeBack(t, before, idx)
	})
}

// dbRead is the test-only whole-index read shim.
func (b *Backend) dbRead(ctx context.Context, fn func(*VMIndex) error) error {
	return b.view(ctx, func(t *vmTx) error {
		_, idx, err := materialize(t)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

func materialize(t *vmTx) (*VMIndex, *VMIndex, error) {
	idx := &VMIndex{}
	idx.Init()
	var err error
	if idx.VMs, err = t.All(); err != nil {
		return nil, nil, err
	}
	if err := t.r.ScanRaw(t.ctx, t.ns, tableNames, func(name string, _ json.RawMessage) error {
		id, ok, err := t.NameGet(name)
		if err != nil || !ok {
			return err
		}
		idx.Names[name] = id
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if idx.OrphanDirs, err = t.orphanDirs(); err != nil {
		return nil, nil, err
	}
	snapshot := &VMIndex{VMs: maps.Clone(idx.VMs), Names: maps.Clone(idx.Names), OrphanDirs: append([]string(nil), idx.OrphanDirs...)}
	return snapshot, idx, nil
}

func writeBack(t *vmTx, before, after *VMIndex) error {
	for id := range before.VMs {
		if after.VMs[id] == nil {
			if err := t.Del(id); err != nil {
				return err
			}
		}
	}
	for id, rec := range after.VMs {
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
	for _, dir := range before.OrphanDirs {
		if err := t.removeOrphanDir(dir); err != nil {
			return err
		}
	}
	for _, dir := range after.OrphanDirs {
		if err := t.addOrphanDir(dir); err != nil {
			return err
		}
	}
	return nil
}
