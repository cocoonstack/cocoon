package vm

import (
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
				ResourceConfig: types.ResourceConfig{
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
				ResourceConfig: types.ResourceConfig{
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

	snap := takeSnapshot(vms[0])
	if snap.id != "1" || snap.name != "earlier" || snap.image != "img-a" {
		t.Fatalf("takeSnapshot() = %+v", snap)
	}
}
