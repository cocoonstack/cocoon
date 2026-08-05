package core

import (
	"testing"

	"github.com/spf13/cobra"

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
	snapCfg := types.SnapshotConfig{Config: types.Config{
		CPU: 2, Memory: 1 << 30, Storage: 10 << 30,
		CPUWeight: 9999, CPUQuotaUs: 999999, CPUPeriodUs: 100000, CPUBurstUs: 999999,
	}}

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

func TestCloneVMConfigKnobFlagsOverrideSnapshot(t *testing.T) {
	snapCfg := types.SnapshotConfig{Config: types.Config{
		CPU: 2, Memory: 1 << 30, Storage: 10 << 30,
		CPUWeight: 40, CPUQuotaUs: 200000, CPUBurstUs: 50000,
	}}

	tests := []struct {
		name       string
		set        map[string]string
		wantWeight int
		wantQuota  int64
		wantBurst  int64
		wantErr    bool
	}{
		{name: "inherit all", wantWeight: 40, wantQuota: 200000, wantBurst: 50000},
		{name: "override weight and burst", set: map[string]string{"cpu-weight": "10", "cpu-burst-us": "100000"}, wantWeight: 10, wantQuota: 200000, wantBurst: 100000},
		{name: "invalid override rejected", set: map[string]string{"cpu-weight": "20000"}, wantErr: true},
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
		})
	}
}
