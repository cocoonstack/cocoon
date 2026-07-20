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

func TestValidateRestoreMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		mem     chMemory
		wantErr bool
	}{
		{"empty mode any mem", "", chMemory{HugePages: true, Shared: true}, false},
		{"copy any mem", "copy", chMemory{HugePages: true}, false},
		{"ondemand hugepages", "ondemand", chMemory{HugePages: true}, false},
		{"mmap plain", "mmap", chMemory{}, false},
		{"mmap hugepages", "mmap", chMemory{HugePages: true}, true},
		{"mmap shared", "mmap", chMemory{Shared: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRestoreMode(tt.mode, tt.mem)
			if (err != nil) != tt.wantErr {
				t.Errorf("got err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}
