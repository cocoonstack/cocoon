package vm

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

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
			NetSetup: types.NetSetup{
				NetworkConfigs: []*types.NetworkConfig{
					{Network: &types.Network{IP: "10.0.0.2"}},
					{Network: &types.Network{IP: "10.0.0.3"}},
				},
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
				if err := renderVMList(tt.vms, tt.format, t.TempDir()); err != nil {
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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close() //nolint:errcheck
	defer w.Close() //nolint:errcheck // idempotent fallback if fn panics before the inline Close
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
