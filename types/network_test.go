package types

import "testing"

func TestValidateNetworkConfigs(t *testing.T) {
	if err := ValidateNetworkConfigs(nil); err != nil {
		t.Errorf("empty: %v", err)
	}
	if err := ValidateNetworkConfigs([]*NetworkConfig{{TAP: "tap0"}}); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := ValidateNetworkConfigs([]*NetworkConfig{{TAP: "tap0"}, nil}); err == nil {
		t.Error("nil entry must be rejected")
	}
}
