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

func TestSnapshotSourceNormalize(t *testing.T) {
	tests := []struct {
		name    string
		src     SnapshotSource
		wantErr bool
	}{
		{"valid", SnapshotSource{Dir: "/snap", Name: "snap-abcd"}, false},
		{"missing dir", SnapshotSource{Name: "snap-abcd"}, true},
		{"missing name", SnapshotSource{Dir: "/snap"}, true},
		{"path in name", SnapshotSource{Dir: "/snap", Name: "a/../b"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.src.Normalize()
			if (err != nil) != tt.wantErr {
				t.Errorf("Normalize() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSnapshotName(t *testing.T) {
	tests := []struct {
		snapID, want string
	}{
		{"7QXKZ2MVB4N5LTRWYHC3DAEFGJ", "snap-7qxkz2mv"},
		{"ABC", "snap-abc"},
	}
	for _, tt := range tests {
		got := SnapshotName(tt.snapID)
		if got != tt.want {
			t.Errorf("SnapshotName(%q) = %q, want %q", tt.snapID, got, tt.want)
		}
		if err := ValidateName(got); err != nil {
			t.Errorf("SnapshotName(%q) = %q is not a valid disk name: %v", tt.snapID, got, err)
		}
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
