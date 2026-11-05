package cloudhypervisor

import (
	"encoding/json/jsontext"
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
	"github.com/cocoonstack/cocoon/types"
)

func TestSnapshotPauseRefusesHotAttachedBeforePausing(t *testing.T) {
	tests := []struct {
		name    string
		config  chVMConfig
		wantErr bool
	}{
		{"vhost-user-fs", chVMConfig{Fs: []chFs{{ID: "cocoon-fs-data", Tag: "data"}}}, true},
		{"vfio device", chVMConfig{Devices: []chDevice{{ID: "gpu", Path: "/sys/bus/pci/devices/0000:01:00.0"}}}, true},
		{"hot-attached disk", chVMConfig{Disks: []chDisk{{ID: "cocoon-disk-vol1", Path: "/vols/a.raw"}}}, true},
		{"only recorded disks", chVMConfig{Disks: []chDisk{{ID: "_disk0", Path: "/run/vm/cow.raw"}}}, false},
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
			hc := &http.Client{Transport: handlerTransport{mux}}

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

func TestSnapshotPauseRefusesANICCountOffItsRecord(t *testing.T) {
	pauses := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.MarshalWrite(w, chVMInfoResponse{State: chStateRunning, Config: chVMConfig{Nets: []chNet{{ID: "net0"}}}})
	})
	mux.HandleFunc("/api/v1/vm.pause", func(w http.ResponseWriter, _ *http.Request) {
		pauses++
		w.WriteHeader(http.StatusNoContent)
	})
	hc := &http.Client{Transport: handlerTransport{mux}}
	rec := &hypervisor.VMRecord{}
	rec.NetworkConfigs = []*types.NetworkConfig{{}, {}}

	err := (&CloudHypervisor{}).snapshotSpec(t.Context()).Pause(rec, hc)

	if err == nil || !strings.Contains(err.Error(), "rerun vm net") {
		t.Fatalf("err = %v, want the capture refused until vm net converges", err)
	}
	if pauses != 0 {
		t.Fatalf("pauses = %d, want the VM left running", pauses)
	}
}

func TestSnapshotPauseWaitsForAPendingNICEject(t *testing.T) {
	tests := []struct {
		name    string
		tree    func(polls, removes int) []string
		wait    time.Duration
		removes int
		gone    string
	}{
		{"nothing pending", func(int, int) []string { return []string{"cocoon-net-new"} }, 0, 0, ""},
		{"guest acks the eject", func(polls, _ int) []string {
			if polls < 4 {
				return []string{"cocoon-net-new", "_net3"}
			}
			return []string{"cocoon-net-new"}
		}, 300 * time.Millisecond, 0, ""},
		{"a dropped eject request acks after the re-request", func(_, removes int) []string {
			if removes == 0 {
				return []string{"cocoon-net-new", "_net3"}
			}
			return []string{"cocoon-net-new"}
		}, ejectRenotifyInterval + 100*time.Millisecond, 1, ""},
		{"a node the guest never ejects stops at the cap", func(int, int) []string { return []string{"cocoon-net-new", "_net3"} }, ejectWaitTimeout, 6, ""},
		{"a node ejected before its re-request does not end the wait", func(_, removes int) []string {
			if removes < 2 {
				return []string{"cocoon-net-new", "_net3", "_net4"}
			}
			return []string{"cocoon-net-new"}
		}, ejectRenotifyInterval + 100*time.Millisecond, 2, "_net3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				polls, removes, pauses := 0, 0, 0
				mux := http.NewServeMux()
				mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
					tree := map[string]jsontext.Value{}
					for _, id := range tt.tree(polls, removes) {
						tree[id] = jsontext.Value(`{}`)
					}
					_ = json.MarshalWrite(w, chVMInfoResponse{
						State:      chStateRunning,
						Config:     chVMConfig{Nets: []chNet{{ID: "cocoon-net-new"}}},
						DeviceTree: tree,
					})
					polls++
				})
				mux.HandleFunc("/api/v1/vm.remove-device", func(w http.ResponseWriter, r *http.Request) {
					removes++
					var req struct {
						ID string `json:"id"`
					}
					_ = json.UnmarshalRead(r.Body, &req)
					if req.ID == tt.gone {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				})
				mux.HandleFunc("/api/v1/vm.pause", func(w http.ResponseWriter, _ *http.Request) {
					pauses++
					w.WriteHeader(http.StatusNoContent)
				})
				hc := &http.Client{Transport: handlerTransport{mux}}
				rec := &hypervisor.VMRecord{NetworkConfigs: []*types.NetworkConfig{{}}}
				start := time.Now()

				if err := (&CloudHypervisor{}).snapshotSpec(t.Context()).Pause(rec, hc); err != nil {
					t.Fatalf("Pause: %v", err)
				}
				if got := time.Since(start); got != tt.wait {
					t.Errorf("paused after %s, want %s", got, tt.wait)
				}
				if removes != tt.removes {
					t.Errorf("eject re-requests = %d, want %d", removes, tt.removes)
				}
				if pauses != 1 {
					t.Errorf("pauses = %d, want 1", pauses)
				}
			})
		})
	}
}

func TestSnapshotPauseWaitsForTheWindowsReplacementNICToStart(t *testing.T) {
	tests := []struct {
		name     string
		windows  bool
		txAfter  int
		wait     time.Duration
		counters bool
	}{
		{"linux pauses at the eject ack", false, 1 << 30, 100 * time.Millisecond, false},
		{"windows waits for the first frame", true, 3, 400 * time.Millisecond, true},
		{"windows NIC that never starts stops at the cap", true, 1 << 30, ejectWaitTimeout, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				infoPolls, ctrPolls := 0, 0
				mux := http.NewServeMux()
				mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
					tree := map[string]jsontext.Value{"cocoon-net-new": jsontext.Value(`{}`)}
					if infoPolls < 2 {
						tree["_net3"] = jsontext.Value(`{}`)
					}
					_ = json.MarshalWrite(w, chVMInfoResponse{
						State:      chStateRunning,
						Config:     chVMConfig{Nets: []chNet{{ID: "cocoon-net-new"}}},
						DeviceTree: tree,
					})
					infoPolls++
				})
				mux.HandleFunc("/api/v1/vm.counters", func(w http.ResponseWriter, _ *http.Request) {
					tx := uint64(0)
					if ctrPolls >= tt.txAfter {
						tx = 1
					}
					_ = json.MarshalWrite(w, chCounters{"cocoon-net-new": {"tx_frames": tx}, "_disk0": {"read_ops": 9}})
					ctrPolls++
				})
				mux.HandleFunc("/api/v1/vm.pause", func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				})
				hc := &http.Client{Transport: handlerTransport{mux}}
				rec := &hypervisor.VMRecord{NetworkConfigs: []*types.NetworkConfig{{}}}
				rec.Config.Windows = tt.windows
				start := time.Now()

				if err := (&CloudHypervisor{}).snapshotSpec(t.Context()).Pause(rec, hc); err != nil {
					t.Fatalf("Pause: %v", err)
				}
				if got := time.Since(start); got != tt.wait {
					t.Errorf("paused after %s, want %s", got, tt.wait)
				}
				if (ctrPolls > 0) != tt.counters {
					t.Errorf("vm.counters polls = %d, want polled %v", ctrPolls, tt.counters)
				}
			})
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
						Config:           chVMConfig{Memory: chMemory{Size: mem}, Balloon: tt.balloon},
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
	dir := t.TempDir()
	cfg := `{"disks":[{"path":"/vols/a.raw","id":"other-id"}]}`
	if err := os.WriteFile(filepath.Join(dir, configJSONName), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := buildSnapshotMeta(&hypervisor.VMRecord{}, dir)
	if err == nil || !strings.Contains(err.Error(), "not present in VM record") {
		t.Fatalf("err = %v, want a record error", err)
	}
}

type handlerTransport struct{ h http.Handler }

func (ht handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	ht.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}
