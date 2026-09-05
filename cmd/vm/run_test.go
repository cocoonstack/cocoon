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

func TestCloneNICPlan(t *testing.T) {
	pci := types.SnapshotConfig{Config: types.Config{PCI: true}, NICs: 1}
	mmio := types.SnapshotConfig{NICs: 1}
	tests := []struct {
		name       string
		useFC      bool
		cfg        types.SnapshotConfig
		override   bool
		target     int
		wantInit   int
		wantResize int
		wantErr    bool
	}{
		{name: "inherit", useFC: true, cfg: pci, wantInit: 1, wantResize: 1},
		{name: "cloud hypervisor swaps during restore", cfg: mmio, override: true, target: 3, wantInit: 3, wantResize: 3},
		{name: "firecracker pci restores then resizes", useFC: true, cfg: pci, override: true, target: 3, wantInit: 1, wantResize: 3},
		{name: "firecracker mmio rejected", useFC: true, cfg: mmio, override: true, target: 3, wantErr: true},
		{name: "negative rejected", useFC: true, cfg: pci, override: true, target: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			init, resize, err := cloneNICPlan(tt.useFC, tt.cfg, tt.override, tt.target)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (init != tt.wantInit || resize != tt.wantResize) {
				t.Fatalf("plan = (%d, %d), want (%d, %d)", init, resize, tt.wantInit, tt.wantResize)
			}
		})
	}
}
