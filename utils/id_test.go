package utils

import (
	"testing"
)

func TestValidID(t *testing.T) {
	if !ValidID(GenerateID()) {
		t.Error("a generated ID is invalid")
	}
	for _, id := range []string{"", "short", "../../../../../../../etc/pw", "abcdefghijklmnopqrstuvwxyz", "AAAAAAAAAAAAAAAAAAAAAAAAA1"} {
		if ValidID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestGenerateID_Length(t *testing.T) {
	id := GenerateID()
	if len(id) != 26 {
		t.Errorf("length: got %d, want 26", len(id))
	}
}

func TestGenerateID_Base32Chars(t *testing.T) {
	id := GenerateID()
	for _, c := range id {
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			t.Errorf("non-base32 character: %c", c)
		}
	}
}

func TestGenerateID_Uniqueness(t *testing.T) {
	seen := make(map[string]struct{})
	for range 100 {
		id := GenerateID()
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = struct{}{}
	}
}
