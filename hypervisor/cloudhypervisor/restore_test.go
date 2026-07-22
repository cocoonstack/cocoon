package cloudhypervisor

import (
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestValidateRestoreNICs(t *testing.T) {
	cfg := func(macs ...string) *chVMConfig {
		c := &chVMConfig{}
		for i, m := range macs {
			c.Nets = append(c.Nets, chNet{TAP: "tap" + string(rune('0'+i)), MAC: m})
		}
		return c
	}
	rec := func(macs ...string) *hypervisor.VMRecord {
		r := &hypervisor.VMRecord{}
		for _, m := range macs {
			r.NetworkConfigs = append(r.NetworkConfigs, &types.NetworkConfig{MAC: m})
		}
		return r
	}

	if err := validateRestoreNICs(cfg("aa:bb:cc:dd:ee:01"), rec("AA:BB:CC:DD:EE:01")); err != nil {
		t.Fatalf("case-insensitive match must pass: %v", err)
	}
	if err := validateRestoreNICs(cfg("aa:bb:cc:dd:ee:01"), rec("aa:bb:cc:dd:ee:02")); err == nil {
		t.Fatal("MAC drift (net resize) must be rejected")
	}
	if err := validateRestoreNICs(cfg("aa:bb:cc:dd:ee:01"), rec()); err == nil {
		t.Fatal("NIC count drift must be rejected")
	}
}

func TestResolveRestoreMode(t *testing.T) {
	tests := []struct {
		name string
		mode string
		mem  chMemory
		want string
	}{
		{"default plain gets mmap", "", chMemory{}, "mmap"},
		{"default hugepages stays copy", "", chMemory{HugePages: true}, ""},
		{"default shared stays copy", "", chMemory{Shared: true}, ""},
		{"explicit copy wins", "copy", chMemory{}, "copy"},
		{"copy any mem", "copy", chMemory{HugePages: true}, "copy"},
		{"explicit ondemand wins", "ondemand", chMemory{}, "ondemand"},
		{"ondemand hugepages", "ondemand", chMemory{HugePages: true}, "ondemand"},
		{"mmap plain", "mmap", chMemory{}, "mmap"},
		{"mmap hugepages degrades to copy", "mmap", chMemory{HugePages: true}, "copy"},
		{"mmap shared degrades to copy", "mmap", chMemory{Shared: true}, "copy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveRestoreMode(t.Context(), tt.mode, tt.mem); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
