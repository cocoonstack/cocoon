package cloudhypervisor

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cocoonstack/cocoon/config"
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

func TestRestoreAndResumeCloneResetsTheBalloonTarget(t *testing.T) {
	const def = balloonTestMem / 4
	tests := []struct {
		name       string
		snap       *chBalloon
		noBalloon  bool
		keepPaused bool
		want       []string
	}{
		{name: "inflated snapshot", snap: &chBalloon{Size: balloonTestMem / 2}, want: []string{"vm.resize", "vm.resume"}},
		{name: "snapshot at the default", snap: &chBalloon{Size: def}, want: []string{"vm.resume"}},
		{name: "no balloon device", want: []string{"vm.resume"}},
		{name: "record without a balloon", snap: &chBalloon{Size: balloonTestMem / 2}, noBalloon: true, want: []string{"vm.resume"}},
		{name: "migration receive keeps the guest's target", snap: &chBalloon{Size: balloonTestMem / 2}, keepPaused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				calls   []string
				resized []int64
			)
			sock := serveCHHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				endpoint := filepath.Base(r.URL.Path)
				if endpoint == "vm.restore" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls = append(calls, endpoint)
				if endpoint == "vm.resize" {
					var body chResize
					_ = json.UnmarshalRead(r.Body, &body)
					resized = append(resized, body.DesiredBalloon)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			scopeParent := t.TempDir()
			if err := os.Mkdir(filepath.Join(scopeParent, "vm-T.scope"), 0o755); err != nil {
				t.Fatalf("setup: %v", err)
			}
			ch := &CloudHypervisor{
				Backend: &hypervisor.Backend{Conf: tmpScopeConf{Config: NewConfig(&config.Config{}), parent: scopeParent}},
				conf:    NewConfig(&config.Config{}),
			}
			if err := ch.restoreAndResumeClone(t.Context(), 0, sock, t.TempDir(), &cloneResumeOpts{
				vmID:        "T",
				vmCfg:       &types.VMConfig{CPU: 2, Memory: balloonTestMem, NoBalloon: tt.noBalloon},
				directBoot:  true,
				snapshotCfg: &chVMConfig{Balloon: tt.snap},
				keepPaused:  tt.keepPaused,
			}); err != nil {
				t.Fatalf("restoreAndResumeClone: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(calls, tt.want) {
				t.Errorf("calls after vm.restore = %v, want %v", calls, tt.want)
			}
			if len(resized) > 0 && resized[0] != def {
				t.Errorf("desired_balloon = %d, want the default %d", resized[0], def)
			}
		})
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
