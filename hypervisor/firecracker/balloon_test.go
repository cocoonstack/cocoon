package firecracker

import (
	"errors"
	"testing"

	"github.com/cocoonstack/cocoon/extend/balloon"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestResizeBalloon(t *testing.T) {
	const memMiB = 4096
	tests := []struct {
		name      string
		noBalloon bool
		paused    bool
		spec      balloon.Spec
		wantBody  string
		wantMiB   int
		wantErr   bool
		wantIs    error
	}{
		{name: "percent of guest memory", spec: balloon.Spec{Percent: 50}, wantBody: `{"amount_mib":2048}`, wantMiB: 2048},
		{name: "bytes round down to MiB", spec: balloon.Spec{Size: 3<<30 + 4095}, wantBody: `{"amount_mib":3072}`, wantMiB: 3072},
		{name: "zero deflates fully", wantBody: `{"amount_mib":0}`},
		{name: "no balloon device", noBalloon: true, spec: balloon.Spec{Percent: 50}, wantErr: true, wantIs: balloon.ErrUnsupported},
		{name: "not below guest memory", spec: balloon.Spec{Size: memMiB << 20}, wantErr: true},
		{name: "paused guest stays paused", paused: true, spec: balloon.Spec{Percent: 50}, wantErr: true, wantIs: hypervisor.ErrNotRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fcVMConfig{MachineConfig: fcMachineConfig{MemSizeMiB: memMiB}}
			if !tt.noBalloon {
				cfg.Balloon = &fcBalloon{AmountMiB: memMiB / 4, DeflateOnOOM: true}
			}
			srv := newFCStub(t, cfg)
			if tt.paused {
				srv.setState(vmStatePaused)
			}
			rec := &hypervisor.VMRecord{ID: "vm1", Config: types.VMConfig{Memory: memMiB << 20}}

			st, err := resizeBalloon(t.Context(), srv.client, rec, tt.spec)
			if got := srv.put("/balloon"); got != tt.wantBody {
				t.Errorf("PATCH /balloon body = %q, want %q", got, tt.wantBody)
			}
			if tt.wantErr {
				if err == nil || (tt.wantIs != nil && !errors.Is(err, tt.wantIs)) {
					t.Fatalf("err = %v, want a refusal matching %v", err, tt.wantIs)
				}
				if srv.resumes() != 0 {
					t.Fatalf("resumed the guest %d times, want it left as found", srv.resumes())
				}
				return
			}
			if err != nil {
				t.Fatalf("resizeBalloon: %v", err)
			}
			want := balloon.Status{Target: int64(tt.wantMiB) << 20, Default: memMiB << 20 / 4}
			if st != want {
				t.Errorf("status = %+v, want %+v", st, want)
			}
			live, err := getVMConfig(t.Context(), srv.client)
			if err != nil || live.Balloon.AmountMiB != tt.wantMiB || !live.Balloon.DeflateOnOOM {
				t.Errorf("live balloon = %+v, %v; want target %d MiB with deflate-on-OOM kept", live.Balloon, err, tt.wantMiB)
			}
		})
	}
}

func TestResetCloneBalloon(t *testing.T) {
	const memMiB = 4096
	tests := []struct {
		name      string
		live      *fcBalloon
		noBalloon bool
		wantBody  string
	}{
		{name: "inflated snapshot", live: &fcBalloon{AmountMiB: memMiB / 2}, wantBody: `{"amount_mib":1024}`},
		{name: "snapshot at the default", live: &fcBalloon{AmountMiB: memMiB / 4}},
		{name: "no balloon device", live: nil},
		{name: "record without a balloon", live: &fcBalloon{AmountMiB: memMiB / 2}, noBalloon: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFCStub(t, fcVMConfig{Balloon: tt.live, MachineConfig: fcMachineConfig{MemSizeMiB: memMiB}})
			cfg := &types.Config{Memory: memMiB << 20, NoBalloon: tt.noBalloon}
			if err := resetCloneBalloon(t.Context(), srv.client, cfg); err != nil {
				t.Fatalf("resetCloneBalloon: %v", err)
			}
			if got := srv.put("/balloon"); got != tt.wantBody {
				t.Errorf("PATCH /balloon body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}
