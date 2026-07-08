package types

import (
	"strings"
	"testing"
)

func TestValidDataDiskName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"data1", true},
		{"d", true},
		{"a-b_c", true},
		{strings.Repeat("a", 20), true},
		{strings.Repeat("a", 21), false},
		{"", false},
		{"1abc", false},
		{"Data", false},
		{"data!", false},
		{"cocoon-cow", false},
		{"cocoon-anything", false},
	}
	for _, tt := range tests {
		if got := ValidDataDiskName(tt.name); got != tt.want {
			t.Errorf("ValidDataDiskName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestValidateStorageConfigs(t *testing.T) {
	tBool := func(b bool) *bool { return &b }
	tests := []struct {
		name    string
		configs []*StorageConfig
		wantErr string
	}{
		{
			name: "ok layer cow data cidata",
			configs: []*StorageConfig{
				{Path: "/a", RO: true, Role: StorageRoleLayer, Serial: "l0"},
				{Path: "/b", RO: false, Role: StorageRoleCOW, Serial: "cocoon-cow"},
				{Path: "/c", RO: false, Role: StorageRoleData, Serial: "data1", FSType: "ext4", MountPoint: "/mnt/x"},
				{Path: "/d", RO: true, Role: StorageRoleCidata, Serial: ""},
			},
		},
		{
			name:    "missing role",
			configs: []*StorageConfig{{Path: "/a", RO: true}},
			wantErr: "must be one of",
		},
		{
			name:    "layer must be RO",
			configs: []*StorageConfig{{Path: "/a", RO: false, Role: StorageRoleLayer}},
			wantErr: "RO=true",
		},
		{
			name:    "cow must be RW",
			configs: []*StorageConfig{{Path: "/a", RO: true, Role: StorageRoleCOW}},
			wantErr: "RO=false",
		},
		{
			name:    "data invalid serial",
			configs: []*StorageConfig{{Path: "/a", RO: false, Role: StorageRoleData, Serial: "Invalid", FSType: "ext4"}},
			wantErr: "name rules",
		},
		{
			name:    "data fstype unknown",
			configs: []*StorageConfig{{Path: "/a", RO: false, Role: StorageRoleData, Serial: "d", FSType: "xfs"}},
			wantErr: "fstype",
		},
		{
			name:    "fstype none with mount",
			configs: []*StorageConfig{{Path: "/a", RO: false, Role: StorageRoleData, Serial: "d", FSType: "none", MountPoint: "/mnt/x"}},
			wantErr: "mount_point empty",
		},
		{
			name:    "mount must be absolute",
			configs: []*StorageConfig{{Path: "/a", RO: false, Role: StorageRoleData, Serial: "d", FSType: "ext4", MountPoint: "relative/path"}},
			wantErr: "absolute",
		},
		{
			name:    "DirectIO only on data",
			configs: []*StorageConfig{{Path: "/a", RO: false, Role: StorageRoleCOW, DirectIO: tBool(true)}},
			wantErr: "direct_io",
		},
		{
			name:    "nil entry",
			configs: []*StorageConfig{nil},
			wantErr: "nil",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStorageConfigs(tt.configs)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
