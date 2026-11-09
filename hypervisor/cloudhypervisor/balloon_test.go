package cloudhypervisor

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cocoonstack/cocoon/extend/balloon"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

const (
	balloonTestMem  = int64(4 << 30)
	balloonTestStep = int64(512 << 20)
)

func TestResizeBalloon(t *testing.T) {
	tests := []struct {
		name      string
		noBalloon bool
		paused    bool
		spec      balloon.Spec
		want      []int64
		wantErr   bool
		wantIs    error
	}{
		{name: "percent of guest memory", spec: balloon.Spec{Percent: 50}, want: []int64{balloonTestMem / 2}},
		{name: "bytes", spec: balloon.Spec{Size: 3 << 30}, want: []int64{3 << 30}},
		{name: "zero deflates fully", want: []int64{0}},
		{name: "no balloon device", noBalloon: true, spec: balloon.Spec{Percent: 50}, wantErr: true, wantIs: balloon.ErrUnsupported},
		{name: "not below guest memory", spec: balloon.Spec{Size: balloonTestMem}, wantErr: true},
		{name: "paused guest stays paused", paused: true, spec: balloon.Spec{Percent: 50}, wantErr: true, wantIs: hypervisor.ErrNotRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vmm := newFakeBalloonVMM(tt.noBalloon)
			if tt.paused {
				vmm.state = chStatePaused
			}
			hc := &http.Client{Transport: handlerTransport{vmm.handler()}}
			rec := balloonTestRecord()
			info, err := getVMInfo(t.Context(), hc)
			if err != nil {
				t.Fatalf("vm.info: %v", err)
			}

			st, err := resizeBalloon(t.Context(), hc, rec, info, tt.spec)
			if !slices.Equal(vmm.resizes, tt.want) {
				t.Errorf("desired_balloon sent %v, want %v", vmm.resizes, tt.want)
			}
			if tt.wantErr {
				if err == nil || (tt.wantIs != nil && !errors.Is(err, tt.wantIs)) {
					t.Fatalf("err = %v, want a refusal matching %v", err, tt.wantIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("resizeBalloon: %v", err)
			}
			if st.Target != tt.want[0] || st.Actual == nil || *st.Actual != balloonTestMem/4 || st.Default != balloonTestMem/4 {
				t.Errorf("status = %+v (actual %v), want target %d actual and default %d", st, st.Actual, tt.want[0], balloonTestMem/4)
			}
		})
	}
}

func TestSnapshotPauseWaitsForTheResizedBalloonTarget(t *testing.T) {
	tests := []struct {
		name   string
		resize *balloon.Spec
		wait   time.Duration
	}{
		{"boot target already held", nil, 0},
		{"inflated target", &balloon.Spec{Percent: 50}, 2 * balloonPollInterval},
		{"deflated target", &balloon.Spec{}, 0},
		{"target off a page boundary", &balloon.Spec{Percent: 30}, balloonPollInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				vmm := newFakeBalloonVMM(false)
				hc := &http.Client{Transport: handlerTransport{vmm.handler()}}
				if tt.resize != nil {
					info, err := getVMInfo(t.Context(), hc)
					if err != nil {
						t.Fatalf("vm.info: %v", err)
					}
					if _, err := resizeBalloon(t.Context(), hc, balloonTestRecord(), info, *tt.resize); err != nil {
						t.Fatalf("resizeBalloon: %v", err)
					}
				}
				start := time.Now()

				if err := (&CloudHypervisor{}).snapshotSpec(t.Context()).Pause(&hypervisor.VMRecord{}, hc); err != nil {
					t.Fatalf("pause: %v", err)
				}
				if elapsed := time.Since(start); elapsed != tt.wait {
					t.Fatalf("waited %s, want %s", elapsed, tt.wait)
				}
			})
		})
	}
}

func TestPatchCHConfigKeepsAnInflatedBalloonTarget(t *testing.T) {
	cfg := baseCHConfig()
	inflated := int64(512 << 20)
	cfg["balloon"].(map[string]any)["size"] = inflated
	path := writeCHConfig(t, t.TempDir(), cfg)

	if err := patchCHConfig(path, basePatchOpts()); err != nil {
		t.Fatalf("patchCHConfig: %v", err)
	}
	got := readRawJSON(t, path)["balloon"].(map[string]any)["size"]
	if got != float64(inflated) {
		t.Errorf("balloon.size = %v, want %d", got, inflated)
	}
}

// fakeBalloonVMM moves the balloon one step toward its target after each vm.info, in whole 4 KiB pages like CH.
type fakeBalloonVMM struct {
	mu        sync.Mutex
	state     string
	noBalloon bool
	size      int64
	held      int64
	resizes   []int64
}

func newFakeBalloonVMM(noBalloon bool) *fakeBalloonVMM {
	return &fakeBalloonVMM{state: chStateRunning, noBalloon: noBalloon, size: balloonTestMem / 4, held: balloonTestMem / 4}
}

func (f *fakeBalloonVMM) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		info := chVMInfoResponse{State: f.state, Config: chVMConfig{Memory: chMemory{Size: balloonTestMem}}, MemoryActualSize: balloonTestMem - f.held}
		if !f.noBalloon {
			info.Config.Balloon = &chBalloon{Size: f.size}
		}
		_ = json.MarshalWrite(w, info)
		f.held = min(f.size>>12<<12, f.held+balloonTestStep)
	})
	mux.HandleFunc("/api/v1/vm.resize", func(w http.ResponseWriter, r *http.Request) {
		var body chResize
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.size = body.DesiredBalloon
		f.resizes = append(f.resizes, body.DesiredBalloon)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/vm.pause", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func balloonTestRecord() *hypervisor.VMRecord {
	return &hypervisor.VMRecord{ID: "vm1", Config: types.VMConfig{Memory: balloonTestMem}}
}
