package cloudhypervisor

import (
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/extend/disk"
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
