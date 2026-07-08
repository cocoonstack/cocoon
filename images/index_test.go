package images

import (
	"testing"
	"time"
)

type testEntry struct {
	id  string
	ref string
}

func (e testEntry) EntryID() string           { return e.id }
func (e testEntry) EntryRef() string          { return e.ref }
func (e testEntry) EntryCreatedAt() time.Time { return time.Time{} }
func (e testEntry) DigestHexes() []string     { return nil }

func TestLookupOne(t *testing.T) {
	sameA := &testEntry{id: "sha256:aabb0000111122223333444455556666", ref: "img:v1"}
	sameB := &testEntry{id: "sha256:aabb0000111122223333444455556666", ref: "img:v2"}
	other := &testEntry{id: "sha256:aabb9999888877776666555544443333", ref: "other:v1"}
	images := map[string]*testEntry{
		"img:v1":   sameA,
		"img:v2":   sameB,
		"other:v1": other,
	}

	if _, e, ok := LookupOne(images, "img:v1"); !ok || e != sameA {
		t.Errorf("exact ref lookup failed: ok=%v", ok)
	}
	// Unique digest prefix resolves.
	if ref, _, ok := LookupOne(images, "aabb999988887777"); !ok || ref != "other:v1" {
		t.Errorf("unique prefix: ok=%v ref=%q", ok, ref)
	}
	// Tag aliases of one digest are not ambiguous.
	if _, e, ok := LookupOne(images, "sha256:aabb0000111122223333444455556666"); !ok || (*e).EntryID() != sameA.id {
		t.Errorf("multi-tag single-digest must resolve: ok=%v", ok)
	}
	// A prefix spanning two distinct digests must resolve to nothing.
	if _, _, ok := LookupOne(images, "aabb"); ok {
		t.Error("ambiguous cross-digest prefix must not resolve")
	}
}
