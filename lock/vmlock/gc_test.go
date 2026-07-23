package vmlock

import (
	"os"
	"testing"
)

type fakeActive map[string]struct{}

func (f fakeActive) ActiveVMIDs() map[string]struct{} { return f }

func TestGCModuleSweepsOnlyUnknownUnheldLocks(t *testing.T) {
	root := t.TempDir()
	ctx := t.Context()
	for _, id := range []string{"active", "stale", "held"} {
		l, err := New(root, id)
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if err := l.Lock(ctx); err != nil {
			t.Fatalf("seed lock %s: %v", id, err)
		}
		if id == "held" {
			defer l.Unlock(ctx) //nolint:errcheck
			continue
		}
		// Recreate the file after the transient unlock removed it, standing in for a pre-transient install's residue.
		if err := l.Unlock(ctx); err != nil {
			t.Fatalf("seed unlock %s: %v", id, err)
		}
		if err := os.WriteFile(Path(root, id), nil, 0o600); err != nil {
			t.Fatalf("recreate %s: %v", id, err)
		}
	}

	m := GCModule(root)
	snap, err := m.ReadDB(ctx)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	ids := m.Resolve(ctx, snap, map[string]any{"hv": fakeActive{"active": {}}})
	if err := m.Collect(ctx, ids, snap); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if _, err := os.Stat(Path(root, "active")); err != nil {
		t.Error("an active VM's lock file was swept")
	}
	if _, err := os.Stat(Path(root, "held")); err != nil {
		t.Error("a held lock file was swept")
	}
	if _, err := os.Stat(Path(root, "stale")); !os.IsNotExist(err) {
		t.Errorf("stale lock file survived the sweep: %v", err)
	}
}

func TestOpsLockUnlinksOnRelease(t *testing.T) {
	root := t.TempDir()
	ctx := t.Context()
	l, err := New(root, "vm1")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(root, "vm1")); err != nil {
		t.Fatalf("lock file must exist while held: %v", err)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(root, "vm1")); !os.IsNotExist(err) {
		t.Errorf("lock file survived release: %v", err)
	}
}
