package hypervisor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestValidateSnapshotIntegrity(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("cow.raw")
	mustWrite("cidata.img")
	mustWrite("data-mydata.raw")

	sidecar := []*types.StorageConfig{
		{Path: "/blob/abc.erofs", RO: true, Role: types.StorageRoleLayer, Serial: "layer0"},
		{Path: "/src/runDir/cow.raw", RO: false, Role: types.StorageRoleCOW, Serial: CowSerial},
		{Path: "/src/runDir/data-mydata.raw", RO: false, Role: types.StorageRoleData, Serial: "mydata", FSType: "ext4"},
		{Path: "/src/runDir/cidata.img", RO: true, Role: types.StorageRoleCidata, Serial: ""},
	}

	t.Run("ok", func(t *testing.T) {
		if err := ValidateSnapshotIntegrity(dir, sidecar); err != nil {
			t.Errorf("expected ok, got %v", err)
		}
	})

	t.Run("missing data disk", func(t *testing.T) {
		bogus := slices.Clone(sidecar)
		bogus = append(bogus, &types.StorageConfig{
			Path: "/src/runDir/data-missing.raw", RO: false, Role: types.StorageRoleData,
			Serial: "missing", FSType: "ext4",
		})
		if err := ValidateSnapshotIntegrity(dir, bogus); err == nil {
			t.Error("expected error for missing data disk")
		}
	})

	t.Run("missing cow", func(t *testing.T) {
		empty := t.TempDir()
		if err := ValidateSnapshotIntegrity(empty, sidecar[:2]); err == nil {
			t.Error("expected error for missing cow.raw")
		}
	})

	t.Run("invalid sidecar rejected by ValidateStorageConfigs", func(t *testing.T) {
		// Layer with RO=false fails the structural check before file stat.
		bad := []*types.StorageConfig{
			{Path: "/x", RO: false, Role: types.StorageRoleLayer, Serial: "x"},
		}
		err := ValidateSnapshotIntegrity(dir, bad)
		if err == nil || !strings.Contains(err.Error(), "RO=true") {
			t.Errorf("expected structural error, got %v", err)
		}
	})

	t.Run("layers skipped (not in srcDir)", func(t *testing.T) {
		// Only a Layer entry; its blob path is not under dir, but Layer is skipped.
		layerOnly := []*types.StorageConfig{
			{Path: "/some/other/blob.erofs", RO: true, Role: types.StorageRoleLayer, Serial: "l"},
		}
		if err := ValidateSnapshotIntegrity(dir, layerOnly); err != nil {
			t.Errorf("layer entries must not require srcDir presence: %v", err)
		}
	})
}

func TestValidateRoleSequence(t *testing.T) {
	tests := []struct {
		name    string
		sidecar []*types.StorageConfig
		rec     []*types.StorageConfig
		wantErr string
	}{
		{
			name: "exact match",
			sidecar: []*types.StorageConfig{
				{Role: types.StorageRoleLayer},
				{Role: types.StorageRoleCOW},
				{Role: types.StorageRoleData},
			},
			rec: []*types.StorageConfig{
				{Role: types.StorageRoleLayer},
				{Role: types.StorageRoleCOW},
				{Role: types.StorageRoleData},
			},
		},
		{
			name: "rec has trailing cidata not in sidecar",
			sidecar: []*types.StorageConfig{
				{Role: types.StorageRoleCOW},
				{Role: types.StorageRoleData},
			},
			rec: []*types.StorageConfig{
				{Role: types.StorageRoleCOW},
				{Role: types.StorageRoleData},
				{Role: types.StorageRoleCidata},
			},
		},
		{
			name: "sidecar longer than rec",
			sidecar: []*types.StorageConfig{
				{Role: types.StorageRoleCOW},
				{Role: types.StorageRoleData},
			},
			rec:     []*types.StorageConfig{{Role: types.StorageRoleCOW}},
			wantErr: "snapshot has",
		},
		{
			name: "role mismatch at index",
			sidecar: []*types.StorageConfig{
				{Role: types.StorageRoleCOW},
			},
			rec: []*types.StorageConfig{
				{Role: types.StorageRoleData},
			},
			wantErr: "role mismatch",
		},
		{
			name:    "rec extension is not cidata",
			sidecar: []*types.StorageConfig{{Role: types.StorageRoleCOW}},
			rec: []*types.StorageConfig{
				{Role: types.StorageRoleCOW},
				{Role: types.StorageRoleData},
			},
			wantErr: "must be cidata",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRoleSequence(tt.sidecar, tt.rec)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestDataDiskBaseName(t *testing.T) {
	if got := DataDiskBaseName("foo"); got != "data-foo.raw" {
		t.Errorf("got %q, want data-foo.raw", got)
	}
}

func TestIsDataDiskFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"data-foo.raw", true},
		{"data-.raw", true},
		{"data.raw", false},
		{"foo-data.raw", false},
		{"data-foo.img", false},
		{"cow.raw", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsDataDiskFile(tt.name); got != tt.want {
			t.Errorf("IsDataDiskFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBuildBaseCmdline(t *testing.T) {
	// Locks the cmdline format so a refactor of the shared builder can't silently
	// shift kernel boot parameters for either backend.
	const (
		chPrefix = "console=hvc0 loglevel=3"
		fcPrefix = "console=ttyS0 reboot=k loglevel=3 pci=off i8042.noaux 8250.nr_uarts=1"
	)
	nics := []*types.NetworkConfig{
		{Network: &types.Network{IP: "10.0.0.2", Gateway: "10.0.0.1", Prefix: 24}},
	}

	tests := []struct {
		name, prefix, layers, cow string
		nics                      []*types.NetworkConfig
		dns                       []string
		want                      string
	}{
		{
			name: "ch no network", prefix: chPrefix, layers: "L0,L1", cow: "cow",
			want: "console=hvc0 loglevel=3 boot=cocoon-overlay cocoon.layers=L0,L1 cocoon.cow=cow clocksource=kvm-clock rw",
		},
		{
			name: "fc no network", prefix: fcPrefix, layers: "/dev/vda", cow: "/dev/vdb",
			want: "console=ttyS0 reboot=k loglevel=3 pci=off i8042.noaux 8250.nr_uarts=1 boot=cocoon-overlay cocoon.layers=/dev/vda cocoon.cow=/dev/vdb clocksource=kvm-clock rw",
		},
		{
			name: "ch with nic + dns", prefix: chPrefix, layers: "L", cow: "C", nics: nics, dns: []string{"1.1.1.1"},
			want: "console=hvc0 loglevel=3 boot=cocoon-overlay cocoon.layers=L cocoon.cow=C clocksource=kvm-clock rw net.ifnames=0 cocoon.hostname=vm ip=10.0.0.2::10.0.0.1:255.255.255.0:vm:eth0:off:1.1.1.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildBaseCmdline(tt.prefix, tt.layers, tt.cow, tt.nics, "vm", tt.dns)
			if got != tt.want {
				t.Errorf("BuildBaseCmdline mismatch:\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}
