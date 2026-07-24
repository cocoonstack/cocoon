package hypervisor

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/lock/vmlock"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/types"
)

var testVMTables = metajson.TableCodec{Specs: []metajson.TableSpec{
	{Key: "vms", Table: TableRecords},
	{Key: "names", Table: TableNames},
	{Key: "orphan_dirs", Table: TableOrphanDirs, Optional: true, StringList: true},
	{Key: "tombstones", Table: tombstone.TableName, Optional: true},
}}

// TestLegacyDifferentialTrace replays the fixture op sequence over meta-json
// and requires byte-identical output to what the LEGACY storage layer wrote
// for the same operations (fixtures generated at master by cmd/fixturegen).
func TestLegacyDifferentialTrace(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	baseline, err := os.ReadFile("testdata/legacy-vms.baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile("testdata/legacy-vms.after.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "vms.json")
	if err := os.WriteFile(path, baseline, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := metajson.Open(metajson.Namespace{
		Name: "vms_test", FilePath: path, LockPath: filepath.Join(dir, "vms.lock"), Codec: testVMTables,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	b := &Backend{Typ: "test", NS: "vms_test", Meta: store}

	// Fidelity: a no-op transaction must reproduce the legacy bytes exactly.
	if err := b.update(ctx, func(*vmTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(baseline) {
		t.Fatalf("round-trip not byte-identical to legacy baseline:\n got: %s\nwant: %s", got, baseline)
	}

	// Replay the exact op sequence fixturegen ran through the legacy store.
	t2 := time.Date(2026, 7, 3, 12, 45, 0, 0, time.UTC)
	if err := b.update(ctx, func(t *vmTx) error {
		g, err := t.Get("VMCCCCCCCCCCCCCCCCCCCCCCCC")
		if err != nil {
			return err
		}
		g.State = types.VMStateCreated
		g.UpdatedAt = t2
		if err := t.Put("VMCCCCCCCCCCCCCCCCCCCCCCCC", g); err != nil {
			return err
		}
		if err := t.NameDel("beta"); err != nil {
			return err
		}
		if err := t.Del("VMBBBBBBBBBBBBBBBBBBBBBBBB"); err != nil {
			return err
		}
		if err := t.removeOrphanDir("/mnt/zz-migrated/VMOLD1"); err != nil {
			return err
		}
		return t.addOrphanDir("/mnt/mm-migrated/VMOLD3")
	}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(after) {
		t.Fatalf("differential trace diverged from legacy bytes:\n got: %s\nwant: %s", got, after)
	}
}

// TestCrossComponentVMLockPath asserts CH/FC (via LockVMOps) and CNI (via
// lock/vmlock directly) resolve one vmID to the SAME lock file — a stale
// backend-specific resolver would let CNI GC and VM lifecycle interleave.
func TestCrossComponentVMLockPath(t *testing.T) {
	ctx := t.Context()
	b, _ := newMeteringTestBackend(t)
	const id = "VMLOCKPATHCHECK"

	unlock, err := b.LockVMOps(ctx, id)
	if err != nil {
		t.Fatalf("LockVMOps: %v", err)
	}
	defer unlock()

	peer, err := vmlock.New(b.Conf.RootDirPath(), id)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := peer.TryLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		_ = peer.Unlock(ctx)
		t.Fatal("vmlock path acquired while LockVMOps held — resolvers diverged")
	}
	if _, statErr := os.Stat(vmlock.Path(b.Conf.RootDirPath(), id)); statErr != nil {
		t.Fatalf("lock file not at the shared path: %v", statErr)
	}
}

// VMIndex mirrors the legacy whole-index shape for shim-based tests.
type VMIndex struct {
	VMs        map[string]*VMRecord `json:"vms"`
	Names      map[string]string    `json:"names"`
	OrphanDirs []string             `json:"orphan_dirs,omitempty"`
}

func (idx *VMIndex) Init() {
	if idx.VMs == nil {
		idx.VMs = map[string]*VMRecord{}
	}
	if idx.Names == nil {
		idx.Names = map[string]string{}
	}
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

// addOrphanDir survives only for fixtures/shims: production writes cleanup
// intent through tombstone payloads now.
func (t *vmTx) addOrphanDir(dir string) error {
	if _, ok, err := t.w.GetRaw(t.ctx, t.ns, TableOrphanDirs, dir); err != nil || ok {
		return err
	}
	return t.w.PutRaw(t.ctx, t.ns, TableOrphanDirs, dir, json.RawMessage(`{}`), false)
}

// newTestMetaStore opens a meta store over the given index paths for one backend type.
func newTestMetaStore(t *testing.T, typ, indexFile, lockPath string) *metajson.Store {
	t.Helper()
	store, err := metajson.Open(metajson.Namespace{Name: VMNamespaceName(typ), FilePath: indexFile, LockPath: lockPath, Codec: testVMTables})
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// testNamespace builds a standalone namespace under dir for shim-level tests.
func testNamespace(t *testing.T, typ, dir string) *metajson.Store {
	t.Helper()
	store, err := metajson.Open(metajson.Namespace{Name: VMNamespaceName(typ), FilePath: filepath.Join(dir, "index.json"), LockPath: filepath.Join(dir, "index.lock"), Codec: testVMTables})
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func materialize(t *vmTx) (*VMIndex, *VMIndex, error) {
	idx := &VMIndex{}
	idx.Init()
	var err error
	if idx.VMs, err = t.All(); err != nil {
		return nil, nil, err
	}
	if err := t.r.ScanRaw(t.ctx, t.ns, TableNames, func(name string, _ json.RawMessage) error {
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
	snapshot := &VMIndex{VMs: maps.Clone(idx.VMs), Names: maps.Clone(idx.Names), OrphanDirs: slices.Clone(idx.OrphanDirs)}
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
