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

func TestParseDataDiskSpec(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(*testing.T, DataDiskSpec)
	}{
		{
			name:  "size only",
			input: "size=1G",
			check: func(t *testing.T, s DataDiskSpec) {
				if s.Size != 1<<30 {
					t.Errorf("size: got %d", s.Size)
				}
				if s.MountPointSet {
					t.Error("MountPointSet must be false when mount= absent")
				}
			},
		},
		{
			name:  "all fields",
			input: "size=2G,name=db,fstype=ext4,mount=/mnt/db,directio=on",
			check: func(t *testing.T, s DataDiskSpec) {
				if s.Name != "db" || s.MountPoint != "/mnt/db" || !s.MountPointSet {
					t.Errorf("got %+v", s)
				}
				if s.DirectIO == nil || !*s.DirectIO {
					t.Errorf("directio: got %+v", s.DirectIO)
				}
			},
		},
		{
			name:  "directio off",
			input: "size=20M,directio=off",
			check: func(t *testing.T, s DataDiskSpec) {
				if s.DirectIO == nil || *s.DirectIO {
					t.Errorf("directio: got %+v", s.DirectIO)
				}
			},
		},
		{
			name:  "directio auto leaves nil",
			input: "size=20M,directio=auto",
			check: func(t *testing.T, s DataDiskSpec) {
				if s.DirectIO != nil {
					t.Errorf("directio auto must be nil, got %+v", s.DirectIO)
				}
			},
		},
		{
			name:  "explicit empty mount",
			input: "size=20M,mount=",
			check: func(t *testing.T, s DataDiskSpec) {
				if s.MountPoint != "" || !s.MountPointSet {
					t.Errorf("expected MountPointSet=true MountPoint=\"\", got %+v", s)
				}
			},
		},
		{name: "missing size", input: "name=foo", wantErr: true},
		{name: "size below minimum", input: "size=8M", wantErr: true},
		{name: "reserved prefix", input: "size=20M,name=cocoon-foo", wantErr: true},
		{name: "invalid name", input: "size=20M,name=BadName", wantErr: true},
		{name: "name too long", input: "size=20M,name=this_name_is_way_too_long_for_virtio", wantErr: true},
		{name: "fstype xfs rejected", input: "size=20M,fstype=xfs", wantErr: true},
		{name: "directio invalid", input: "size=20M,directio=maybe", wantErr: true},
		{name: "unknown key", input: "size=20M,bogus=1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := ParseDataDiskSpec(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, spec)
			}
		})
	}
}
