package images

import (
	"path/filepath"
	"testing"

	metajson "github.com/cocoonstack/cocoon/meta/json"
)

type testEntry struct {
	Digest string `json:"digest"`
}

// TestGCCollectSkipsRepublishedBlob pins the loose-GC revalidation: a digest
// that became referenced after the snapshot (a publish finished and released
// its lock) must survive Collect.
func TestGCCollectSkipsRepublishedBlob(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	engine, err := metajson.Open(MetaNamespace[testEntry]("images_test", filepath.Join(dir, "images.json"), filepath.Join(dir, "images.lock")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	store := NewMetaStore[testEntry](engine, "images_test")

	var removed []string
	mod := BuildGCModule(GCModuleConfig[testEntry]{
		Name:     "test",
		Store:    store,
		LockPath: func(hex string) string { return filepath.Join(dir, hex+".lock") },
		ReadRefs: func(m map[string]*testEntry) map[string]struct{} {
			refs := map[string]struct{}{}
			for _, e := range m {
				refs[e.Digest] = struct{}{}
			}
			return refs
		},
		ScanDisk: func() ([]string, error) { return []string{"deadbeef"}, nil },
		Removers: []func(string) error{func(hex string) error { removed = append(removed, hex); return nil }},
		TempDir:  dir,
	})

	// The publish lands between the (empty) snapshot and Collect.
	if err := store.Update(ctx, func(idx *Index[testEntry]) error {
		idx.Images["ref1"] = &testEntry{Digest: "deadbeef"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Collect(ctx, []string{"deadbeef"}, ImageGCSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("republished blob removed: %v", removed)
	}

	// Once the ref is gone the same candidate collects.
	if err := store.Update(ctx, func(idx *Index[testEntry]) error {
		delete(idx.Images, "ref1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Collect(ctx, []string{"deadbeef"}, ImageGCSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "deadbeef" {
		t.Fatalf("unreferenced blob not collected: %v", removed)
	}
}
