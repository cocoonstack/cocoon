package vm

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestMatchesFilter(t *testing.T) {
	vm := &types.VM{
		ID: "abcdef123456",
		Config: types.VMConfig{
			Name: "demo-vm",
		},
	}

	tests := []struct {
		name    string
		filters []string
		want    bool
	}{
		{name: "match exact id", filters: []string{"abcdef123456"}, want: true},
		{name: "match exact name", filters: []string{"demo-vm"}, want: true},
		{name: "match id prefix with enough chars", filters: []string{"abc"}, want: true},
		{name: "reject short prefix", filters: []string{"ab"}, want: false},
		{name: "reject unrelated", filters: []string{"other"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesFilter(vm, tt.filters); got != tt.want {
				t.Fatalf("matchesFilter(%v) = %v, want %v", tt.filters, got, tt.want)
			}
		})
	}
}

func TestVMIPsAndSort(t *testing.T) {
	now := time.Now()
	vms := []*types.VM{
		{
			ID: "2",
			Config: types.VMConfig{
				Name: "later",
				Config: types.Config{
					CPU:    2,
					Memory: 2 << 30,
					Image:  "img-b",
				},
			},
			CreatedAt: now,
		},
		{
			ID: "1",
			Config: types.VMConfig{
				Name: "earlier",
				Config: types.Config{
					CPU:    1,
					Memory: 1 << 30,
					Image:  "img-a",
				},
			},
			CreatedAt: now.Add(-time.Minute),
			NetworkConfigs: []*types.NetworkConfig{
				{Network: &types.Network{IP: "10.0.0.2"}},
				{Network: &types.Network{IP: "10.0.0.3"}},
			},
		},
	}

	sortVMs(vms)
	if vms[0].ID != "1" {
		t.Fatalf("sortVMs() first ID = %s, want 1", vms[0].ID)
	}

	if got := vmIPs(vms[0]); got != "10.0.0.2,10.0.0.3" {
		t.Fatalf("vmIPs() = %q, want %q", got, "10.0.0.2,10.0.0.3")
	}

	snap := takeSnapshot(vms[0], "running")
	if snap.id != "1" || snap.name != "earlier" || snap.image != "img-a" {
		t.Fatalf("takeSnapshot() = %+v", snap)
	}
}

func TestRenderVMList(t *testing.T) {
	vm := &types.VM{
		ID:        "abc",
		Config:    types.VMConfig{Name: "demo", Config: types.Config{CPU: 1, Memory: 1 << 30, Image: "img"}},
		CreatedAt: time.Now(),
	}

	tests := []struct {
		name    string
		vms     []*types.VM
		format  string
		want    string
		notWant string
	}{
		{name: "empty table → No VMs found", vms: nil, format: "", want: "No VMs found."},
		{name: "empty json → []", vms: nil, format: "json", want: "[]"},
		{name: "table with vm contains name", vms: []*types.VM{vm}, format: "", want: "demo"},
		{name: "json with vm contains id", vms: []*types.VM{vm}, format: "json", want: `"id": "abc"`},
		{name: "table mode skips json marker", vms: []*types.VM{vm}, format: "", notWant: `"id":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				if err := renderVMList(tt.vms, tt.format, t.TempDir(), nil); err != nil {
					t.Fatalf("renderVMList: %v", err)
				}
			})
			if tt.want != "" && !strings.Contains(out, tt.want) {
				t.Errorf("output %q does not contain %q", out, tt.want)
			}
			if tt.notWant != "" && strings.Contains(out, tt.notWant) {
				t.Errorf("output %q unexpectedly contains %q", out, tt.notWant)
			}
		})
	}
}

func TestApplyFilters(t *testing.T) {
	vms := []*types.VM{
		{ID: "abcdef123456", Config: types.VMConfig{Name: "alpha"}},
		{ID: "beadbeef0000", Config: types.VMConfig{Name: "beta"}},
	}
	tests := []struct {
		name    string
		filters []string
		wantIDs []string
	}{
		{name: "no filter returns all", filters: nil, wantIDs: []string{"abcdef123456", "beadbeef0000"}},
		{name: "exact name", filters: []string{"alpha"}, wantIDs: []string{"abcdef123456"}},
		{name: "id prefix", filters: []string{"bead"}, wantIDs: []string{"beadbeef0000"}},
		{name: "multi filter union", filters: []string{"alpha", "bead"}, wantIDs: []string{"abcdef123456", "beadbeef0000"}},
		{name: "no match", filters: []string{"zzz"}, wantIDs: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyFilters(vms, tt.filters)
			gotIDs := make([]string, 0, len(got))
			for _, vm := range got {
				gotIDs = append(gotIDs, vm.ID)
			}
			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("applyFilters(%v) = %v, want %v", tt.filters, gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestVMEventCarriesStale(t *testing.T) {
	vm := &types.VM{ID: "v1", State: types.VMStateStopped}
	out, err := json.Marshal(vmEvent{Event: "MODIFIED", VM: vmOutput{VM: vm, Stale: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"MODIFIED"`, `"vm":{"id":"v1"`, `"state":"stopped"`, `"stale":true`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("json %s lacks %s", out, want)
		}
	}
	if out, _ = json.Marshal(vmEvent{Event: "ADDED", VM: vmOutput{VM: vm}}); strings.Contains(string(out), "stale") {
		t.Errorf("a live record must not carry a stale key: %s", out)
	}
}

func TestVMOutputCarriesStale(t *testing.T) {
	vm := &types.VM{ID: "v1", State: types.VMStateStopped, PID: 4242}
	out, err := json.Marshal(vmOutput{VM: vm, Stale: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"state":"stopped"`, `"stale":true`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("json %s lacks %s", out, want)
		}
	}
	if out, _ = json.Marshal(vmOutput{VM: vm}); strings.Contains(string(out), "stale") {
		t.Errorf("a live record must not carry a stale key: %s", out)
	}
}

func TestStatusWatchRefreshesThrottlingWithoutVMChanges(t *testing.T) {
	scopeDir := t.TempDir()
	statDir := cgroup.ScopeDir(scopeDir, "vm-1")
	if err := os.MkdirAll(statDir, 0o750); err != nil {
		t.Fatalf("create scope: %v", err)
	}
	statPath := filepath.Join(statDir, "cpu.stat")
	if err := os.WriteFile(statPath, []byte("nr_throttled 1\nthrottled_usec 1000\n"), 0o600); err != nil {
		t.Fatalf("write initial cpu.stat: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var writeErr error
	h := &statusWatchHypervisor{
		vm: &types.VM{ID: "vm-1", State: types.VMStateRunning, PID: os.Getpid()},
		onList: func(n int) {
			if n == 2 {
				writeErr = os.WriteFile(statPath, []byte("nr_throttled 2\nthrottled_usec 2000\n"), 0o600)
				cancel()
			}
		},
	}
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	out := captureStdout(t, func() {
		statusRefreshLoop(ctx, []hypervisor.Hypervisor{h}, nil, nil, tick, true, scopeDir)
	})
	if writeErr != nil {
		t.Fatalf("update cpu.stat: %v", writeErr)
	}
	for _, want := range []string{"1/1ms", "2/2ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output %q lacks %q", out, want)
		}
	}
	if got := strings.Count(out, "\033[H\033[2J"); got != 2 {
		t.Errorf("screen redraws = %d, want 2", got)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	orig := os.Stdout
	defer func() { os.Stdout = orig }()
	os.Stdout = w

	var buf []byte
	done := make(chan struct{})
	go func() {
		buf, _ = io.ReadAll(r)
		close(done)
	}()

	fn()
	_ = w.Close()
	<-done
	return string(buf)
}

type statusWatchHypervisor struct {
	hypervisor.Hypervisor
	vm     *types.VM
	onList func(int)
	calls  int
}

func (h *statusWatchHypervisor) List(context.Context) ([]*types.VM, error) {
	h.calls++
	h.onList(h.calls)
	return []*types.VM{h.vm}, nil
}
