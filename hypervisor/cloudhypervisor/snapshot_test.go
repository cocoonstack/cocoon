package cloudhypervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func TestBuildSnapshotMetaRefusesUnrecordedDisk(t *testing.T) {
	tests := []struct {
		name    string
		diskID  string
		wantErr string
	}{
		{"hot-attached gets the detach hint", "cocoon-disk-vol1", "detach before snapshot or hibernate"},
		{"foreign unknown disk keeps the record error", "other-id", "not present in VM record"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := `{"disks":[{"path":"/vols/a.raw","id":"` + tt.diskID + `"}]}`
			if err := os.WriteFile(filepath.Join(dir, configJSONName), []byte(cfg), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			rec := &hypervisor.VMRecord{}
			_, err := buildSnapshotMeta(rec, dir)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuildSnapshotMetaRefusesHotAttachedDevices(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		wantErr string
	}{
		{"vhost-user-fs", `{"fs":[{"id":"cocoon-fs-data","tag":"data","socket":"/run/vfsd.sock"}]}`, `hot-attached vhost-user-fs "data": detach before snapshot or hibernate`},
		{"vfio device", `{"devices":[{"id":"cocoon-dev-0","path":"/sys/bus/pci/devices/0000:01:00.0"}]}`, `hot-attached device "/sys/bus/pci/devices/0000:01:00.0": detach before snapshot or hibernate`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, configJSONName), []byte(tt.cfg), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := buildSnapshotMeta(&hypervisor.VMRecord{}, dir)

			if err == nil {
				t.Fatal("snapshot accepted a VM whose device state it cannot restore")
			}
			if !errors.Is(err, hypervisor.ErrHotAttached) || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
