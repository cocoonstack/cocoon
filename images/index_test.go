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
	// Two distinct digests sharing a 16-hex prefix, so an ambiguous query can
	// pass LookupRefs' minHexLen guard and reach the cross-digest check.
	sameA := &testEntry{id: "sha256:aabb000011112222333344445555666a", ref: "img:v1"}
	sameB := &testEntry{id: "sha256:aabb000011112222333344445555666a", ref: "img:v2"}
	other := &testEntry{id: "sha256:aabb000011112222bbbbccccddddeeee", ref: "other:v1"}
	images := map[string]*testEntry{
		"img:v1":   sameA,
		"img:v2":   sameB,
		"other:v1": other,
	}

	if _, e, ok := LookupOne(images, "img:v1"); !ok || e != sameA {
		t.Errorf("exact ref lookup failed: ok=%v", ok)
	}
	// Unique 16-hex digest prefix resolves.
	if ref, _, ok := LookupOne(images, "aabb000011112222bbbb"); !ok || ref != "other:v1" {
		t.Errorf("unique prefix: ok=%v ref=%q", ok, ref)
	}
	// Tag aliases of one digest are not ambiguous.
	if _, e, ok := LookupOne(images, "sha256:aabb000011112222333344445555666a"); !ok || (*e).EntryID() != sameA.id {
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
