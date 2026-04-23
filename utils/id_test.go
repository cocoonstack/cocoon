package utils

import (
	"testing"
)

func TestGenerateID_Length(t *testing.T) {
	id := GenerateID()
	if len(id) != 26 {
		t.Errorf("length: got %d, want 26", len(id))
	}
}

func TestGenerateID_Base32Chars(t *testing.T) {
	id := GenerateID()
	for _, c := range id {
		// crypto/rand.Text uses RFC 4648 base32 alphabet (A-Z, 2-7).
		if !((c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')) {
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
