package json

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/lock/flock"
)

type testData struct {
	Name  string            `json:"name"`
	Count int               `json:"count"`
	Tags  map[string]string `json:"tags"`
}

func (d *testData) Init() {
	if d.Tags == nil {
		d.Tags = make(map[string]string)
	}
}

func newTestStore(t *testing.T, dir, name string) *Store[testData] {
	t.Helper()
	dataPath := filepath.Join(dir, name+".json")
	lockPath := filepath.Join(dir, name+".lock")
	return New[testData](dataPath, flock.New(lockPath))
}

func TestLoadFreshFromDisk(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "data.json")

	// Write directly to disk.
	original := testData{Name: "alice", Count: 1}
	raw, _ := json.Marshal(original)
	if err := os.WriteFile(dataPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	s := New[testData](dataPath, flock.New(filepath.Join(dir, "data.lock")))

	// First read.
	if err := s.ReadRaw(func(d *testData) error {
		if d.Name != "alice" {
			t.Fatalf("expected alice, got %s", d.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Overwrite file behind the store's back (simulating another process).
	updated := testData{Name: "bob", Count: 2}
	raw, _ = json.Marshal(updated)
	if err := os.WriteFile(dataPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Second read must see the new value (no stale cache).
	if err := s.ReadRaw(func(d *testData) error {
		if d.Name != "bob" {
			t.Fatalf("expected bob after disk overwrite, got %s", d.Name)
		}
		if d.Count != 2 {
			t.Fatalf("expected count 2, got %d", d.Count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCrossInstanceVisibility(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	// Two Store instances sharing the same data file and lock file.
	a := newTestStore(t, dir, "shared")
	b := newTestStore(t, dir, "shared")

	// A writes.
	if err := a.Update(ctx, func(d *testData) error {
		d.Name = "from-a"
		d.Count = 42
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// B reads and must see A's write.
	if err := b.With(ctx, func(d *testData) error {
		if d.Name != "from-a" {
			t.Errorf("B expected name=from-a, got %s", d.Name)
		}
		if d.Count != 42 {
			t.Errorf("B expected count=42, got %d", d.Count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTryLockThenReadRaw(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	a := newTestStore(t, dir, "trylock")
	b := newTestStore(t, dir, "trylock")

	// A writes initial data.
	if err := a.Update(ctx, func(d *testData) error {
		d.Name = "initial"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// B acquires via TryLock, uses WriteRaw, then Unlock.
	ok, err := b.TryLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("TryLock should succeed when no one holds it")
	}
	if err := b.WriteRaw(func(d *testData) error {
		d.Name = "from-b"
		d.Count = 99
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock(ctx); err != nil {
		t.Fatal(err)
	}

	// A reads via With and must see B's write.
	if err := a.With(ctx, func(d *testData) error {
		if d.Name != "from-b" {
			t.Errorf("A expected name=from-b, got %s", d.Name)
		}
		if d.Count != 99 {
			t.Errorf("A expected count=99, got %d", d.Count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNonExistentFileReturnsInit(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir, "nonexistent")

	if err := s.ReadRaw(func(d *testData) error {
		if d.Tags == nil {
			t.Error("Init() should have been called, Tags is nil")
		}
		if d.Name != "" {
			t.Errorf("expected empty name, got %s", d.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInitCalledOnDeserialize(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "init.json")

	// Write JSON without tags field.
	if err := os.WriteFile(dataPath, []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New[testData](dataPath, flock.New(filepath.Join(dir, "init.lock")))
	if err := s.ReadRaw(func(d *testData) error {
		if d.Tags == nil {
			t.Error("Init() should have been called after unmarshal")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
