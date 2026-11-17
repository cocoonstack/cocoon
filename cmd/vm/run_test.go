package vm

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestValidateBackendFlagsRejectsPCIOnCloudHypervisor(t *testing.T) {
	vmCfg := &types.VMConfig{PCI: true}
	if err := validateBackendFlags(&config.Config{}, vmCfg); err == nil || !strings.Contains(err.Error(), "--pci") {
		t.Fatalf("err = %v, want a --pci rejection", err)
	}
	if err := validateBackendFlags(&config.Config{UseFirecracker: true}, vmCfg); err != nil {
		t.Fatalf("Firecracker rejects --pci: %v", err)
	}
}

func TestValidateBackendFlagsRejectsFirecrackerIncompatibleConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  types.Config
		want string
	}{
		{"windows", types.Config{Windows: true}, "--windows"},
		{"shared memory", types.Config{SharedMemory: true}, "--shared-memory"},
		{"hugepages", types.Config{HugePages: true}, "--hugepages"},
		{"mergeable", types.Config{Mergeable: true}, "--mergeable"},
		{"no watchdog", types.Config{NoWatchdog: true}, "--no-watchdog"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBackendFlags(&config.Config{UseFirecracker: true}, &types.VMConfig{Config: tt.cfg})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want a %q rejection", err, tt.want)
			}
		})
	}
}

func TestValidateBootCompatRequiresDirectBootOnFirecracker(t *testing.T) {
	conf := &config.Config{UseFirecracker: true}
	if err := validateBootCompat(conf, &types.VMConfig{}, &types.BootConfig{}); err == nil || !strings.Contains(err.Error(), "--fc") {
		t.Fatalf("err = %v, want an --fc rejection", err)
	}
	if err := validateBootCompat(conf, &types.VMConfig{}, &types.BootConfig{KernelPath: "/k"}); err != nil {
		t.Fatalf("direct boot rejected: %v", err)
	}
}

func TestCloneNICPlan(t *testing.T) {
	pci := types.SnapshotConfig{PCI: true, NICs: 1}
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
			initNICs, resize, err := cloneNICPlan(tt.useFC, tt.cfg, tt.override, tt.target)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (initNICs != tt.wantInit || resize != tt.wantResize) {
				t.Fatalf("plan = (%d, %d), want (%d, %d)", initNICs, resize, tt.wantInit, tt.wantResize)
			}
		})
	}
}

func TestCreateRejectsNegativeNICsBeforeResolvingTheImage(t *testing.T) {
	dir := t.TempDir()
	conf := &config.Config{RootDir: dir, RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log")}
	h := Handler{ConfProvider: func() *config.Config { return conf }}
	create := vmSubcommand(t, h, "create", "--nics", "-1")

	_, _, _, err := h.createVM(create, "no-such-image")

	if err == nil || !strings.Contains(err.Error(), "--nics must be non-negative, got -1") {
		t.Fatalf("err = %v, want the same rejection the clone path gives", err)
	}
}

func TestPrepareCloneRejectsFlagsBeforeReserving(t *testing.T) {
	mmio := types.SnapshotConfig{CPU: 1, Memory: 512 << 20, Storage: 10 << 30, NICs: 1}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "nics on a firecracker mmio snapshot", args: []string{"--nics", "2"}, want: "--nics override"},
		{name: "data disk on a firecracker mmio snapshot", args: []string{"--data-disk", "size=1G"}, want: "--data-disk on clone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hyper := &reserveSpyHypervisor{}
			clone := vmSubcommand(t, Handler{}, "clone", tt.args...)

			_, err := Handler{}.prepareClone(t.Context(), clone, &config.Config{UseFirecracker: true}, hyper, mmio)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want a %q rejection", err, tt.want)
			}
			if hyper.locked {
				t.Error("prepareClone took the reservation lock before rejecting the flags")
			}
		})
	}
}

type reserveSpyHypervisor struct {
	hypervisor.Hypervisor
	locked bool
}

func (h *reserveSpyHypervisor) LockVMOps(context.Context, string) (func(), error) {
	h.locked = true
	return nil, errors.New("reservation attempted")
}

func vmSubcommand(t *testing.T, h Handler, name string, args ...string) *cobra.Command {
	t.Helper()
	sub, _, err := Command(h).Find([]string{name})
	if err != nil {
		t.Fatalf("find %s: %v", name, err)
	}
	if err := sub.ParseFlags(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return sub
}
