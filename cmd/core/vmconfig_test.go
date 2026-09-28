package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/types"
)

func TestRestoreModeFromFlags(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		want    string
		wantErr bool
	}{
		{name: "default empty", mode: "", want: ""},
		{name: "copy", mode: "copy", want: "copy"},
		{name: "ondemand", mode: "ondemand", want: "ondemand"},
		{name: "mmap", mode: "mmap", want: "mmap"},
		{name: "invalid", mode: "lazy", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().String("restore-mode", "", "")
			if tt.mode != "" {
				if err := cmd.Flags().Set("restore-mode", tt.mode); err != nil {
					t.Fatalf("set flag: %v", err)
				}
			}
			got, err := restoreModeFromFlags(cmd)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRestoreVMConfigKeepsHostCPUPolicy(t *testing.T) {
	vm := &types.VM{Config: types.VMConfig{
		Name:   "v",
		Config: types.Config{CPU: 1, CPUWeight: 25, CPUQuotaUs: 150000, CPUPeriodUs: 50000, CPUBurstUs: 10000, Network: "keepnet"},
	}}
	snapCfg := types.SnapshotConfig{
		CPU: 2, Memory: 1 << 30, Storage: 10 << 30,
		CPUWeight: 9999, CPUQuotaUs: 999999, CPUPeriodUs: 100000, CPUBurstUs: 999999,
	}

	cmd := &cobra.Command{}
	cmd.Flags().String("restore-mode", "", "")
	got, err := RestoreVMConfigFromFlags(cmd, vm, snapCfg)
	if err != nil {
		t.Fatalf("RestoreVMConfigFromFlags: %v", err)
	}
	if got.CPU != 2 {
		t.Errorf("CPU = %d, want snapshot's 2", got.CPU)
	}
	want := vm.Config
	if got.CPUWeight != want.CPUWeight || got.CPUQuotaUs != want.CPUQuotaUs ||
		got.CPUPeriodUs != want.CPUPeriodUs || got.CPUBurstUs != want.CPUBurstUs {
		t.Errorf("knobs = %d/%d/%d/%d, want the VM's %d/%d/%d/%d",
			got.CPUWeight, got.CPUQuotaUs, got.CPUPeriodUs, got.CPUBurstUs,
			want.CPUWeight, want.CPUQuotaUs, want.CPUPeriodUs, want.CPUBurstUs)
	}
	if got.Network != "keepnet" {
		t.Errorf("Network = %q, want the VM's", got.Network)
	}
}

func TestRestoreVMConfigRejectsKnobsUnfitForSnapshotCPU(t *testing.T) {
	vm := &types.VM{Config: types.VMConfig{
		Name:   "v",
		Config: types.Config{CPU: 2, CPUBurstUs: 150000},
	}}
	snapCfg := types.SnapshotConfig{CPU: 1, Memory: 1 << 30, Storage: 10 << 30}

	cmd := &cobra.Command{}
	cmd.Flags().String("restore-mode", "", "")
	if _, err := RestoreVMConfigFromFlags(cmd, vm, snapCfg); err == nil {
		t.Fatal("want error: kept burst 150000 exceeds the 1-CPU derived quota 100000")
	}
}

func TestCloneVMConfigKnobFlagsOverrideSnapshot(t *testing.T) {
	snapCfg := types.SnapshotConfig{
		CPU: 2, Memory: 1 << 30, Storage: 10 << 30,
		CPUWeight: 40, CPUQuotaUs: 200000, CPUBurstUs: 50000,
		NoWatchdog: true, NoBalloon: true, PCI: true,
	}

	tests := []struct {
		name           string
		set            map[string]string
		wantWeight     int
		wantQuota      int64
		wantBurst      int64
		wantNoWatchdog bool
		wantErr        bool
	}{
		{name: "no flags ignore snapshot knobs", wantWeight: 0, wantQuota: 0, wantBurst: 0, wantNoWatchdog: true},
		{name: "flags set the clone's policy", set: map[string]string{"cpu-weight": "10", "cpu-burst-us": "100000", "cpu-quota-us": "150000"}, wantWeight: 10, wantQuota: 150000, wantBurst: 100000, wantNoWatchdog: true},
		{name: "invalid flag rejected", set: map[string]string{"cpu-weight": "20000"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().String("name", "c", "")
			cmd.Flags().Int("nics", 0, "")
			cmd.Flags().Int("queue-size", 0, "")
			cmd.Flags().Int("disk-queue-size", 0, "")
			cmd.Flags().Int("cpu-weight", 0, "")
			cmd.Flags().Int64("cpu-quota-us", 0, "")
			cmd.Flags().Int64("cpu-period-us", 0, "")
			cmd.Flags().Int64("cpu-burst-us", 0, "")
			cmd.Flags().String("network", "", "")
			cmd.Flags().Bool("no-direct-io", false, "")
			cmd.Flags().String("restore-mode", "", "")
			cmd.Flags().StringArray("data-disk", nil, "")
			for k, v := range tt.set {
				if err := cmd.Flags().Set(k, v); err != nil {
					t.Fatalf("set %s: %v", k, err)
				}
			}
			got, err := CloneVMConfigFromFlags(cmd, snapCfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.CPUWeight != tt.wantWeight || got.CPUQuotaUs != tt.wantQuota || got.CPUBurstUs != tt.wantBurst {
				t.Errorf("knobs = %d/%d/%d, want %d/%d/%d",
					got.CPUWeight, got.CPUQuotaUs, got.CPUBurstUs, tt.wantWeight, tt.wantQuota, tt.wantBurst)
			}
			if got.NoWatchdog != tt.wantNoWatchdog {
				t.Errorf("NoWatchdog = %v, want %v", got.NoWatchdog, tt.wantNoWatchdog)
			}
			if !got.PCI || !got.NoBalloon {
				t.Errorf("PCI/NoBalloon not inherited from the snapshot: %v/%v", got.PCI, got.NoBalloon)
			}
		})
	}
}

func TestEnsureFirmwarePath(t *testing.T) {
	tests := []struct {
		name         string
		arm64        bool
		firecracker  bool
		haveFirmware bool
		boot         types.BootConfig
		wantFirmware bool
		wantErr      bool
	}{
		{name: "arm64 oci gets the node firmware", arm64: true, haveFirmware: true, boot: types.BootConfig{KernelPath: "/boot/Image"}, wantFirmware: true},
		{name: "arm64 oci without firmware fails", arm64: true, boot: types.BootConfig{KernelPath: "/boot/Image"}, wantErr: true},
		{name: "arm64 firecracker oci stays kernel only", arm64: true, firecracker: true, boot: types.BootConfig{KernelPath: "/boot/Image"}},
		{name: "arm64 cloud image keeps its firmware", arm64: true, boot: types.BootConfig{FirmwarePath: "/fw/other.fd"}},
		{name: "x86 oci stays kernel only", haveFirmware: true, boot: types.BootConfig{KernelPath: "/boot/vmlinuz"}},
		{name: "x86 bootless config gets the node firmware", wantFirmware: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := kernelViaFirmware
			kernelViaFirmware = tt.arm64
			t.Cleanup(func() { kernelViaFirmware = orig })

			conf := &config.Config{RootDir: t.TempDir(), UseFirecracker: tt.firecracker}
			firmwarePath := images.FirmwarePath(conf.RootDir)
			if tt.haveFirmware {
				writeFirmware(t, firmwarePath)
			}
			boot := tt.boot
			err := EnsureFirmwarePath(conf, &boot)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), firmwarePath) {
					t.Fatalf("err = %v, want one naming %s", err, firmwarePath)
				}
				if boot != tt.boot {
					t.Errorf("boot = %+v, want it untouched on error", boot)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureFirmwarePath: %v", err)
			}
			want := tt.boot
			if tt.wantFirmware {
				want.FirmwarePath = firmwarePath
			}
			if boot != want {
				t.Errorf("boot = %+v, want %+v", boot, want)
			}
		})
	}
}

func writeFirmware(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(path, []byte("fw"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
}
