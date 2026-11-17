package hypervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/lock/vmlock"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/types"
)

var testVMTables = metajson.TableCodec{Specs: []metajson.TableSpec{
	{Key: "vms", Table: TableRecords},
	{Key: "names", Table: TableNames},
	{Key: "placements", Table: TablePlacements, Optional: true},
	{Key: "tombstones", Table: tombstone.TableName, Optional: true},
}}

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

	updatedAt := time.Date(2026, 7, 3, 12, 45, 0, 0, time.UTC)
	if err := b.update(ctx, func(tx *vmTx) error {
		g, err := tx.Get("VMCCCCCCCCCCCCCCCCCCCCCCCC")
		if err != nil {
			return err
		}
		g.State = types.VMStateCreated
		g.UpdatedAt = updatedAt
		if err := tx.Put("VMCCCCCCCCCCCCCCCCCCCCCCCC", g); err != nil {
			return err
		}
		if err := tx.NameDel("beta"); err != nil {
			return err
		}
		return tx.Del("VMBBBBBBBBBBBBBBBBBBBBBBBB")
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

func newTestMetaStore(t *testing.T, typ, indexFile, lockPath string) *metajson.Store {
	t.Helper()
	store, err := metajson.Open(metajson.Namespace{Name: VMNamespaceName(typ), FilePath: indexFile, LockPath: lockPath, Codec: testVMTables})
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testNamespace(t *testing.T, typ, dir string) meta.Store {
	t.Helper()
	return newTestMetaStore(t, typ, filepath.Join(dir, "index.json"), filepath.Join(dir, "index.lock"))
}

func testSQLiteNamespace(t *testing.T, typ, dir string) meta.Store {
	t.Helper()
	path := filepath.Join(dir, metasqlite.DBFileName)
	decl := metasqlite.Namespace{Name: VMNamespaceName(typ), Tables: []string{TableRecords, TableNames, TablePlacements, tombstone.TableName}}
	if err := metasqlite.Init(t.Context(), path, decl); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	store, err := metasqlite.Open(path, decl)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
