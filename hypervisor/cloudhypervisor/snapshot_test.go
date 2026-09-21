package cloudhypervisor

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func TestSnapshotPauseRefusesHotAttachedBeforePausing(t *testing.T) {
	tests := []struct {
		name    string
		config  chVMInfoConfig
		wantErr bool
	}{
		{"vhost-user-fs", chVMInfoConfig{Fs: []chFs{{ID: "cocoon-fs-data", Tag: "data"}}}, true},
		{"vfio device", chVMInfoConfig{Devices: []chDevice{{ID: "gpu", Path: "/sys/bus/pci/devices/0000:01:00.0"}}}, true},
		{"hot-attached disk", chVMInfoConfig{Disks: []chDisk{{ID: "cocoon-disk-vol1", Path: "/vols/a.raw"}}}, true},
		{"only recorded disks", chVMInfoConfig{Disks: []chDisk{{ID: "_disk0", Path: "/run/vm/cow.raw"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			pauses := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(chVMInfoResponse{State: chStateRunning, Config: tt.config})
			})
			mux.HandleFunc("/api/v1/vm.pause", func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				pauses++
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			})
			hc := newStubHTTPClient(t, mux)

			err := (&CloudHypervisor{}).snapshotSpec(t.Context()).Pause(&hypervisor.VMRecord{}, hc)

			if tt.wantErr != errors.Is(err, hypervisor.ErrHotAttached) {
				t.Fatalf("err = %v, want hot-attached refusal %v", err, tt.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if tt.wantErr && pauses != 0 {
				t.Fatalf("pauses = %d, want the VM left running when the capture is refused", pauses)
			}
			if !tt.wantErr && pauses != 1 {
				t.Fatalf("pauses = %d, want one pause for a clean device set", pauses)
			}
		})
	}
}

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
