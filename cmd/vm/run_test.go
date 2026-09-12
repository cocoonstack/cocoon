package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
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

func TestVerifyFromDirCOWRejectsAShortRawDisk(t *testing.T) {
	tests := []struct {
		name    string
		size    int64
		storage int64
		wantErr bool
	}{
		{"standard tar rebuilt a short cow", 78 << 20, 10 << 30, true},
		{"cocoon export keeps the apparent size", 10 << 30, 10 << 30, false},
		{"envelope records no storage", 78 << 20, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			f, err := os.Create(filepath.Join(dir, hypervisor.COWRawFileName))
			if err != nil {
				t.Fatalf("create cow: %v", err)
			}
			if err = f.Truncate(tt.size); err != nil {
				t.Fatalf("truncate cow: %v", err)
			}
			if err = f.Close(); err != nil {
				t.Fatalf("close cow: %v", err)
			}

			err = verifyFromDirCOW(dir, types.SnapshotConfig{Config: types.Config{Storage: tt.storage}})

			if tt.wantErr && err == nil {
				t.Fatal("accepted a cow.raw the envelope says is bigger")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("rejected a valid export: %v", err)
			}
		})
	}
}

func TestVerifyFromDirCOWSkipsACloudimgOverlay(t *testing.T) {
	if err := verifyFromDirCOW(t.TempDir(), types.SnapshotConfig{Config: types.Config{Storage: 10 << 30}}); err != nil {
		t.Fatalf("rejected a dir with no raw cow: %v", err)
	}
}

func TestInitNetworkRejectsNegativeNICs(t *testing.T) {
	_, _, err := initNetwork(t.Context(), &config.Config{}, "vm1", -1, &types.VMConfig{}, 2, "")

	if err == nil {
		t.Fatal("initNetwork accepted a negative --nics instead of rejecting it")
	}
	if !strings.Contains(err.Error(), "--nics must be non-negative, got -1") {
		t.Errorf("err = %v, want the same rejection the clone path gives", err)
	}
}
