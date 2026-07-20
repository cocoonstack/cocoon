package utils

import (
	"testing"
)

func TestInitNamedIndex_NilMaps(t *testing.T) {
	var items map[string]*int
	var names map[string]string

	InitNamedIndex(&items, &names)

	if items == nil {
		t.Error("items should be initialized")
	}
	if names == nil {
		t.Error("names should be initialized")
	}
}

func TestInitNamedIndex_AlreadyInitialized(t *testing.T) {
	items := map[string]*int{"a": ptr(1)}
	names := map[string]string{"n": "a"}

	InitNamedIndex(&items, &names)

	// Should not reset existing data.
	if len(items) != 1 || items["a"] == nil {
		t.Error("items was reset")
	}
	if len(names) != 1 || names["n"] != "a" {
		t.Error("names was reset")
	}
}

func ptr[T any](v T) *T { return &v }
