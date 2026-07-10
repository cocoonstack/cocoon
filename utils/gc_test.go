package utils

import (
	"testing"
)

func TestCleanStaleRecords_RemovesStale(t *testing.T) {
	type rec struct {
		Name  string
		Stale bool
	}

	items := map[string]*rec{
		"id1": {Name: "vm1", Stale: true},
		"id2": {Name: "vm2", Stale: false},
		"id3": {Name: "vm3", Stale: true},
	}
	names := map[string]string{
		"vm1": "id1",
		"vm2": "id2",
		"vm3": "id3",
	}

	CleanStaleRecords(
		items, names, []string{"id1", "id2", "id3"},
		func(r *rec) string { return r.Name },
		func(r *rec) bool { return r.Stale },
	)

	if len(items) != 1 {
		t.Errorf("items: got %d, want 1", len(items))
	}
	if items["id2"] == nil {
		t.Error("id2 should remain")
	}
	if len(names) != 1 || names["vm2"] != "id2" {
		t.Errorf("names: got %v", names)
	}
}

func TestCleanStaleRecords_NilRecord(t *testing.T) {
	type rec struct{ Name string }

	items := map[string]*rec{
		"id1": {Name: "vm1"},
	}
	names := map[string]string{"vm1": "id1"}

	// Target includes a nil entry.
	CleanStaleRecords(
		items, names, []string{"id1", "id_nil"},
		func(r *rec) string { return r.Name },
		func(_ *rec) bool { return false },
	)

	if len(items) != 1 {
		t.Errorf("items: got %d, want 1", len(items))
	}
}

func TestCleanStaleRecords_EmptyName(t *testing.T) {
	type rec struct{ Name string }

	items := map[string]*rec{
		"id1": {Name: ""},
	}
	names := map[string]string{}

	CleanStaleRecords(
		items, names, []string{"id1"},
		func(r *rec) string { return r.Name },
		func(_ *rec) bool { return true },
	)

	if len(items) != 0 {
		t.Errorf("items should be empty, got %d", len(items))
	}
	if len(names) != 0 {
		t.Errorf("names should be empty, got %d", len(names))
	}
}

func TestCleanStaleRecords_NoTargets(t *testing.T) {
	type rec struct{ Name string }

	items := map[string]*rec{"id1": {Name: "vm1"}}
	names := map[string]string{"vm1": "id1"}

	CleanStaleRecords(
		items, names, nil,
		func(r *rec) string { return r.Name },
		func(_ *rec) bool { return true },
	)

	if len(items) != 1 {
		t.Errorf("expected no changes, got %d items", len(items))
	}
}
