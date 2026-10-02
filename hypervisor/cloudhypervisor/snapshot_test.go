package cloudhypervisor

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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
				_ = json.MarshalWrite(w, chVMInfoResponse{State: chStateRunning, Config: tt.config})
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

func TestSnapshotPauseWaitsForBalloonToSettle(t *testing.T) {
	const (
		mem     = int64(8 << 30)
		balloon = mem / 4
		target  = mem - balloon
	)
	tests := []struct {
		name    string
		state   string
		balloon *chBalloon
		actual  func(polls int) int64
		wait    time.Duration
	}{
		{"settled", chStateRunning, &chBalloon{Size: balloon}, func(int) int64 { return target }, 0},
		{"no balloon", chStateRunning, nil, func(int) int64 { return mem }, 0},
		{"paused guest cannot inflate", chStatePaused, &chBalloon{Size: balloon}, func(int) int64 { return mem }, 0},
		{"inflating reaches target", chStateRunning, &chBalloon{Size: balloon}, func(polls int) int64 { return mem - min(int64(polls)*(512<<20), balloon) }, 4 * balloonPollInterval},
		{"stalled under guest memory pressure", chStateRunning, &chBalloon{Size: balloon}, func(int) int64 { return mem - (512 << 20) }, balloonStallWindow},
		{"slow inflation stops at the cap", chStateRunning, &chBalloon{Size: balloon}, func(polls int) int64 { return mem - int64(polls)<<12 }, balloonSettleTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				polls := 0
				mux := http.NewServeMux()
				mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
					_ = json.MarshalWrite(w, chVMInfoResponse{
						State:            tt.state,
						Config:           chVMInfoConfig{Memory: chMemory{Size: mem}, Balloon: tt.balloon},
						MemoryActualSize: tt.actual(polls),
					})
					polls++
				})
				mux.HandleFunc("/api/v1/vm.pause", func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				})
				hc := &http.Client{Transport: handlerTransport{mux}}
				start := time.Now()

				err := (&CloudHypervisor{}).snapshotSpec(t.Context()).Pause(&hypervisor.VMRecord{}, hc)
				if err != nil {
					t.Fatalf("pause: %v", err)
				}
				if elapsed := time.Since(start); elapsed != tt.wait {
					t.Fatalf("waited %s, want %s", elapsed, tt.wait)
				}
			})
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

type handlerTransport struct{ h http.Handler }

func (ht handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	ht.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}
