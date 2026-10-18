package cloudhypervisor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestDiskAttached(t *testing.T) {
	id := disk.DeriveID("db")
	info := &chVMInfoResponse{}
	info.Config.Disks = []chDisk{
		{ID: "_disk0", Path: "/vm/cow.raw", Serial: "root"},
		{ID: id, Path: "/vols/db.raw", ReadOnly: true, Serial: "db"},
	}
	tests := []struct {
		name, diskName, path string
		readOnly             bool
		want, wantErr        string
	}{
		{"free", "cache", "/vols/cache.raw", false, "", ""},
		{"identical", "db", "/vols/db.raw", true, id, ""},
		{"same name other path", "db", "/vols/other.raw", true, "", "different path or mode"},
		{"same name other mode", "db", "/vols/db.raw", false, "", "different path or mode"},
		{"serial in use", "root", "/vols/root.raw", false, "", "serial"},
		{"same path other name", "db2", "/vols/db.raw", true, "", "already attached as"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := diskAttached(info, disk.DeriveID(tt.diskName), tt.diskName, tt.path, tt.readOnly)
			if got != tt.want {
				t.Errorf("existing id = %q, want %q", got, tt.want)
			}
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckSnapshotImage(t *testing.T) {
	rec := &hypervisor.VMRecord{
		StorageConfigs: []*types.StorageConfig{
			{Path: "/local/blobs/aaa.erofs", RO: true, Role: types.StorageRoleLayer},
			{Path: "/local/blobs/bbb.erofs", RO: true, Role: types.StorageRoleLayer},
			{Path: "/local/run/vm/cow.raw", Role: types.StorageRoleCOW},
		},
		ImageBlobIDs: map[string]struct{}{"aaa": {}, "bbb": {}, "kkk": {}},
	}
	rec.ID = "target-vm"
	tests := []struct {
		name    string
		layers  []string
		blobIDs map[string]struct{}
		wantErr bool
	}{
		{"same image on another root", []string{"/remote/blobs/aaa.erofs", "/remote/blobs/bbb.erofs"}, map[string]struct{}{"aaa": {}, "bbb": {}, "kkk": {}}, false},
		{"layers in another order", []string{"/remote/blobs/bbb.erofs", "/remote/blobs/aaa.erofs"}, map[string]struct{}{"aaa": {}, "bbb": {}, "kkk": {}}, true},
		{"another blob set", []string{"/remote/blobs/aaa.erofs", "/remote/blobs/bbb.erofs"}, map[string]struct{}{"aaa": {}, "bbb": {}, "zzz": {}}, true},
		{"another image", []string{"/remote/blobs/ccc.erofs"}, map[string]struct{}{"ccc": {}, "kkk": {}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sidecar := []*types.StorageConfig{{Path: "/remote/run/vm/cow.raw", Role: types.StorageRoleCOW}}
			for _, p := range tt.layers {
				sidecar = append(sidecar, &types.StorageConfig{Path: p, RO: true, Role: types.StorageRoleLayer})
			}
			err := checkSnapshotImage(sidecar, tt.blobIDs, rec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkSnapshotImage = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && (!strings.Contains(err.Error(), "target-vm") || !strings.Contains(err.Error(), "[aaa bbb]") || !strings.Contains(err.Error(), "[aaa bbb kkk]")) {
				t.Errorf("error %q does not name both sides", err)
			}
		})
	}
}

func TestCheckSnapshotImageCloudimg(t *testing.T) {
	rec := &hypervisor.VMRecord{
		StorageConfigs: []*types.StorageConfig{{Path: "/local/run/vm/overlay.qcow2", Role: types.StorageRoleCOW}},
		ImageBlobIDs:   map[string]struct{}{"base": {}},
	}
	sidecar := []*types.StorageConfig{{Path: "/remote/run/vm/overlay.qcow2", Role: types.StorageRoleCOW}}
	if err := checkSnapshotImage(sidecar, map[string]struct{}{"base": {}}, rec); err != nil {
		t.Errorf("same base rejected: %v", err)
	}
	if err := checkSnapshotImage(sidecar, map[string]struct{}{"other": {}}, rec); err == nil {
		t.Error("another base accepted")
	}
}

func TestSnapshotDiskPathKeepsTheFormat(t *testing.T) {
	tests := []struct {
		cowFile, wantFormat, wantPath string
	}{
		{"cow.raw", formatRaw, "/run/vm/snapdisk-snap-ab.raw"},
		{"overlay.qcow2", formatQcow2, "/run/vm/snapdisk-snap-ab.qcow2"},
	}
	for _, tt := range tests {
		format := snapshotDiskFormat(tt.cowFile)
		if format != tt.wantFormat {
			t.Errorf("snapshotDiskFormat(%q) = %q, want %q", tt.cowFile, format, tt.wantFormat)
		}
		if got := snapshotDiskPath("/run/vm", "snap-ab", format); got != tt.wantPath {
			t.Errorf("snapshotDiskPath = %q, want %q", got, tt.wantPath)
		}
	}
}

func TestRemoveSnapshotDisks(t *testing.T) {
	runDir := t.TempDir()
	writeRunFiles(t, runDir, "snapdisk-a.raw", "snapdisk-a.qcow2", "snapdisk-b.raw", "cow.raw")

	if err := removeSnapshotDisks(runDir, "a"); err != nil {
		t.Fatalf("removeSnapshotDisks: %v", err)
	}
	if err := removeSnapshotDisks(runDir, "a"); err != nil {
		t.Fatalf("repeat removeSnapshotDisks: %v", err)
	}
	if got := runFiles(t, runDir); !slices.Equal(got, []string{"cow.raw", "snapdisk-b.raw"}) {
		t.Errorf("left in run dir = %v, want [cow.raw snapdisk-b.raw]", got)
	}
}

func TestRuntimeFilesSweepSnapshotDisks(t *testing.T) {
	runDir := t.TempDir()
	writeRunFiles(t, runDir, "snapdisk-a.raw", "snapdisk-b.qcow2", pidFileName, "cow.raw", "overlay.qcow2")

	hypervisor.CleanupRuntimeFiles(t.Context(), runDir, runtimeFiles)

	if got := runFiles(t, runDir); !slices.Equal(got, []string{"cow.raw", "overlay.qcow2"}) {
		t.Errorf("left in run dir = %v, want [cow.raw overlay.qcow2]", got)
	}
}

func writeRunFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
}

func runFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
