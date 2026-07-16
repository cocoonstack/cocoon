package images

import (
	"slices"
	"testing"
	"time"
)

func TestLookupOne(t *testing.T) {
	images := testIndex()

	if _, e, ok := LookupOne(images, "img:v1"); !ok || e.id != "sha256:aabb000011112222333344445555666a" {
		t.Errorf("exact ref lookup failed: ok=%v", ok)
	}
	// Unique 16-hex digest prefix resolves.
	if ref, _, ok := LookupOne(images, "aabb000011112222bbbb"); !ok || ref != "other:v1" {
		t.Errorf("unique prefix: ok=%v ref=%q", ok, ref)
	}
	// Tag aliases of one digest are not ambiguous.
	if _, e, ok := LookupOne(images, "sha256:aabb000011112222333344445555666a"); !ok || (*e).EntryID() != "sha256:aabb000011112222333344445555666a" {
		t.Errorf("multi-tag single-digest must resolve: ok=%v", ok)
	}
	// 16-hex prefix spanning two distinct digests: past the minHexLen guard,
	// must be refused by the cross-digest check.
	if _, _, ok := LookupOne(images, "aabb000011112222"); ok {
		t.Error("ambiguous cross-digest prefix must not resolve")
	}
	// Sub-minHexLen prefix never reaches prefix matching at all.
	if _, _, ok := LookupOne(images, "aabb"); ok {
		t.Error("short prefix must not resolve")
	}
}

func TestDeleteByIDRejectsAmbiguousPrefix(t *testing.T) {
	images := testIndex()
	lookup := func(id string) []string { return LookupRefs(images, id) }

	deleted, err := deleteByID(t.Context(), "test.Delete", images, lookup, []string{"aabb000011112222"})
	if err == nil {
		t.Fatal("ambiguous cross-digest prefix must be rejected, not swept")
	}
	if len(deleted) != 0 || len(images) != 3 {
		t.Fatalf("nothing may be deleted on ambiguity: deleted=%v remaining=%d", deleted, len(images))
	}

	// A digest shared only by tag aliases still sweeps all its refs.
	deleted, err = deleteByID(t.Context(), "test.Delete", images, lookup, []string{"sha256:aabb000011112222333344445555666a"})
	if err != nil {
		t.Fatalf("single-digest delete: %v", err)
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"img:v1", "img:v2"}) {
		t.Fatalf("deleted = %v, want both tag aliases", deleted)
	}
}

// testIndex has two distinct digests sharing a 16-hex prefix, so an ambiguous
// query can pass LookupRefs' minHexLen guard and reach the cross-digest check.
func testIndex() map[string]*testEntry {
	return map[string]*testEntry{
		"img:v1":   {id: "sha256:aabb000011112222333344445555666a", ref: "img:v1"},
		"img:v2":   {id: "sha256:aabb000011112222333344445555666a", ref: "img:v2"},
		"other:v1": {id: "sha256:aabb000011112222bbbbccccddddeeee", ref: "other:v1"},
	}
}

type testEntry struct {
	id  string
	ref string
}

func (e testEntry) EntryID() string           { return e.id }
func (e testEntry) EntryRef() string          { return e.ref }
func (e testEntry) EntryCreatedAt() time.Time { return time.Time{} }
func (e testEntry) DigestHexes() []string     { return nil }
