package vm

import (
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/types"
)

func TestValidateBackendFlagsRejectsPCIOnCloudHypervisor(t *testing.T) {
	vmCfg := &types.VMConfig{Config: types.Config{PCI: true}}
	if err := validateBackendFlags(&config.Config{}, vmCfg); err == nil || !strings.Contains(err.Error(), "--pci") {
		t.Fatalf("err = %v, want a --pci rejection", err)
	}
	if err := validateBackendFlags(&config.Config{UseFirecracker: true}, vmCfg); err != nil {
		t.Fatalf("Firecracker rejects --pci: %v", err)
	}
}
