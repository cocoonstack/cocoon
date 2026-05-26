package types

import (
	"strings"
	"testing"
)

func TestSnapshotConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfgName string
		wantErr bool
	}{
		{"empty allowed", "", false},
		{"simple", "my-snap", false},
		{"with dot underscore", "my.snap_v1", false},
		{"oci repo:tag", "namespace/repo:v1", false},
		{"oci repo only", "namespace/repo", false},
		{"colon only", "repo:tag", false},
		{"max 63", strings.Repeat("a", 63), false},
		{"over 63", strings.Repeat("a", 64), true},
		{"leading hyphen", "-bad", true},
		{"leading slash", "/bad", true},
		{"leading colon", ":bad", true},
		{"space", "bad name", true},
		{"semicolon", "bad;rm", true},
		{"backtick", "bad`name", true},
		{"control char", "bad\x00name", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := (&SnapshotConfig{Name: tt.cfgName}).Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate(%q): err=%v, wantErr=%v", tt.cfgName, err, tt.wantErr)
			}
		})
	}
}
