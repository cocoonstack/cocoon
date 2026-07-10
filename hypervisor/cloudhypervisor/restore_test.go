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
