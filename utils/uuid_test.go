package utils

import (
	"strings"
	"testing"
)

func TestUUIDv5_Format(t *testing.T) {
	id := UUIDv5("test-name")

	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("expected 5 parts, got %d: %q", len(parts), id)
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Errorf("unexpected format: %q", id)
	}
}

func TestUUIDv5_Deterministic(t *testing.T) {
	id1 := UUIDv5("same-input")
	id2 := UUIDv5("same-input")
	if id1 != id2 {
		t.Errorf("not deterministic: %q != %q", id1, id2)
	}
}

func TestUUIDv5_DifferentInputs(t *testing.T) {
	id1 := UUIDv5("input-a")
	id2 := UUIDv5("input-b")
	if id1 == id2 {
		t.Errorf("different inputs produced same UUID: %q", id1)
	}
}

func TestUUIDv5_Version5(t *testing.T) {
	id := UUIDv5("version-check")
	// Version 5 UUID has '5' as the 13th character (position in 8-4-4-4-12).
	if id[14] != '5' {
		t.Errorf("expected version 5, got %c at position 14 in %q", id[14], id)
	}
}
