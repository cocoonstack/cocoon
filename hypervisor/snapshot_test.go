package hypervisor

import (
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestIsUnderDir(t *testing.T) {
	tests := []struct {
		name string
		path string
		dir  string
		want bool
	}{
		{"under root", "/srv/cocoon/runs/vm1", "/srv/cocoon", true},
		{"exactly root", "/srv/cocoon", "/srv/cocoon", false},
		{"escape via dotdot", "/srv/cocoon/../etc/shadow", "/srv/cocoon", false},
		{"sibling dir", "/srv/cocoon-other/x", "/srv/cocoon", false},
		{"empty dir disables check", "/anything", "", false},
		{"trailing slash on dir", "/srv/cocoon/x", "/srv/cocoon/", true},
		{"relative path normalized", "srv/cocoon/x/../y", "srv/cocoon", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUnderDir(tt.path, tt.dir); got != tt.want {
				t.Errorf("IsUnderDir(%q, %q) = %v, want %v", tt.path, tt.dir, got, tt.want)
			}
		})
	}
}

func TestValidateMetaPaths_Accepts(t *testing.T) {
	rootDir := "/srv/cocoon"
	runDir := "/run/cocoon"
	meta := &SnapshotMeta{
		StorageConfigs: []*types.StorageConfig{
			{Path: filepath.Join(rootDir, "layers/abc")},
			{Path: filepath.Join(runDir, "vm1/cow.raw")},
		},
		BootConfig: &types.BootConfig{
			KernelPath: filepath.Join(rootDir, "oci/.../boot/vmlinuz"),
			InitrdPath: filepath.Join(rootDir, "oci/.../boot/initrd"),
		},
	}
	if err := ValidateMetaPaths(meta, rootDir, runDir); err != nil {
		t.Fatalf("ValidateMetaPaths: %v", err)
	}
}

func TestValidateMetaPaths_RejectsTraversal(t *testing.T) {
	rootDir := "/srv/cocoon"
	runDir := "/run/cocoon"
	meta := &SnapshotMeta{
		StorageConfigs: []*types.StorageConfig{
			{Path: "/etc/shadow"},
		},
	}
	if err := ValidateMetaPaths(meta, rootDir, runDir); err == nil {
		t.Fatal("expected ValidateMetaPaths to reject /etc/shadow")
	}
}

func TestValidateMetaPaths_RejectsKernelEscape(t *testing.T) {
	meta := &SnapshotMeta{
		BootConfig: &types.BootConfig{KernelPath: "/usr/bin/sudo"},
	}
	if err := ValidateMetaPaths(meta, "/srv/cocoon", "/run/cocoon"); err == nil {
		t.Fatal("expected kernel path outside rootDir to be rejected")
	}
}

func TestValidateMetaPaths_RejectsInitrdEscape(t *testing.T) {
	meta := &SnapshotMeta{
		BootConfig: &types.BootConfig{
			KernelPath: "/srv/cocoon/oci/k",
			InitrdPath: "/etc/passwd",
		},
	}
	if err := ValidateMetaPaths(meta, "/srv/cocoon", "/run/cocoon"); err == nil {
		t.Fatal("expected initrd path outside rootDir to be rejected")
	}
}

func TestCloneStorageConfigs_DeepCopy(t *testing.T) {
	src := []*types.StorageConfig{
		{Path: "/a", Serial: "s1"},
		{Path: "/b", Serial: "s2"},
	}
	got := CloneStorageConfigs(src)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	got[0].Path = "/zzz"
	if src[0].Path != "/a" {
		t.Errorf("source mutated by clone modification: %q", src[0].Path)
	}
}

func TestCloneStorageConfigs_Empty(t *testing.T) {
	got := CloneStorageConfigs(nil)
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

func TestReverseLayers_Order(t *testing.T) {
	configs := []*types.StorageConfig{
		{Role: types.StorageRoleLayer, Serial: "L0"},
		{Role: types.StorageRoleLayer, Serial: "L1"},
		{Role: types.StorageRoleCOW, Serial: "C"},
		{Role: types.StorageRoleLayer, Serial: "L2"},
	}
	got := ReverseLayers(configs, func(_ int, sc *types.StorageConfig) string { return sc.Serial })
	want := []string{"L2", "L1", "L0"}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReverseLayers_IndexCallback(t *testing.T) {
	configs := []*types.StorageConfig{
		{Role: types.StorageRoleLayer},
		{Role: types.StorageRoleLayer},
		{Role: types.StorageRoleLayer},
	}
	got := ReverseLayers(configs, func(idx int, _ *types.StorageConfig) int { return idx })
	want := []int{2, 1, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestReverseLayers_NoLayers(t *testing.T) {
	configs := []*types.StorageConfig{
		{Role: types.StorageRoleCOW},
	}
	got := ReverseLayers(configs, func(_ int, sc *types.StorageConfig) string { return sc.Serial })
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}
