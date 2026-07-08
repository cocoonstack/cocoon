package disk

import "testing"

func TestSpecNormalize(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		wantErr bool
	}{
		{"valid", Spec{Path: "/data/vol1.raw", Name: "vol1"}, false},
		{"missing path", Spec{Name: "vol1"}, true},
		{"relative path", Spec{Path: "vol1.raw", Name: "vol1"}, true},
		{"missing name", Spec{Path: "/data/vol1.raw"}, true},
		{"bad name", Spec{Path: "/data/vol1.raw", Name: "VOL1"}, true},
		{"name too long", Spec{Path: "/d/v.raw", Name: "abcdefghijklmnopqrstu"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Normalize()
			if (err != nil) != tt.wantErr {
				t.Errorf("Normalize() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDeriveIDRoundTrip(t *testing.T) {
	id := DeriveID("vol1")
	if id != "cocoon-disk-vol1" {
		t.Fatalf("DeriveID = %q", id)
	}
	if got := NameFromID(id); got != "vol1" {
		t.Fatalf("NameFromID(%q) = %q, want vol1", id, got)
	}
	if got := NameFromID("cocoon-fs-x"); got != "" {
		t.Fatalf("NameFromID(foreign id) = %q, want empty", got)
	}
}

func TestNameFromIDRejectsForeignSuffix(t *testing.T) {
	for _, id := range []string{"cocoon-disk-VOL1", "cocoon-disk-", "cocoon-disk-9bad", "cocoon-disk-abcdefghijklmnopqrstu"} {
		if got := NameFromID(id); got != "" {
			t.Errorf("NameFromID(%q) = %q, want empty", id, got)
		}
	}
}
