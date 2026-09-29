package cloudhypervisor

import (
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestMemoryCLIArg(t *testing.T) {
	tests := []struct {
		name string
		cfg  types.Config
		want string
	}{
		{name: "plain", cfg: types.Config{Memory: 1 << 30}, want: "size=1073741824"},
		{name: "hugepages+shared", cfg: types.Config{Memory: 1 << 30, HugePages: true, SharedMemory: true}, want: "size=1073741824,hugepages=on,shared=on"},
		{name: "mergeable", cfg: types.Config{Memory: 1 << 30, Mergeable: true}, want: "size=1073741824,mergeable=on"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &hypervisor.VMRecord{Config: types.VMConfig{Config: tt.cfg}}
			args := buildCLIArgs(buildVMConfig(rec, "", nil), "api.sock")
			i := slices.Index(args, "--memory")
			if i < 0 || i+1 >= len(args) || args[i+1] != tt.want {
				t.Fatalf("memory arg not %q (args: %s)", tt.want, strings.Join(args, " "))
			}
		})
	}
}

func TestEffectiveDirectIO(t *testing.T) {
	tests := []struct {
		name       string
		sc         types.StorageConfig
		noDirectIO bool
		want       bool
	}{
		{name: "raw cow", sc: types.StorageConfig{Path: "/v/cow.raw", Role: types.StorageRoleCOW}, want: true},
		{name: "raw cow with no-direct-io", sc: types.StorageConfig{Path: "/v/cow.raw", Role: types.StorageRoleCOW}, noDirectIO: true},
		{name: "readonly layer", sc: types.StorageConfig{Path: "/v/base.raw", RO: true, Role: types.StorageRoleLayer}},
		{name: "qcow2 overlay stays buffered", sc: types.StorageConfig{Path: "/v/overlay.qcow2", Role: types.StorageRoleCOW}},
		{name: "readonly qcow2 has no backing chain", sc: types.StorageConfig{Path: "/v/base.qcow2", RO: true, Role: types.StorageRoleLayer}},
		{name: "explicit override wins", sc: types.StorageConfig{Path: "/v/data.raw", Role: types.StorageRoleData, DirectIO: new(false)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveDirectIO(&tt.sc, tt.noDirectIO); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWatchdogPolicy(t *testing.T) {
	for _, tt := range []struct {
		name       string
		noWatchdog bool
		want       bool
	}{
		{name: "default enabled", want: true},
		{name: "explicitly disabled", noWatchdog: true, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := &hypervisor.VMRecord{Config: types.VMConfig{NoWatchdog: tt.noWatchdog}}
			if got := buildVMConfig(rec, "", nil).Watchdog; got != tt.want {
				t.Fatalf("Watchdog = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQcow2OverlayDiskArgs(t *testing.T) {
	sc := &types.StorageConfig{Path: "/v/overlay.qcow2", Role: types.StorageRoleCOW}
	got := diskToCLIArg(storageConfigToDisk(sc, 1, 0, false, nil))
	if strings.Contains(got, "direct=on") {
		t.Errorf("qcow2 overlay must stay buffered so the shared base keeps one page-cache copy: %s", got)
	}
	if !strings.Contains(got, "backing_files=on") {
		t.Errorf("qcow2 overlay must keep backing_files=on: %s", got)
	}
}

func TestBalloonPolicy(t *testing.T) {
	for _, tt := range []struct {
		name      string
		noBalloon bool
		wantSize  int64
	}{
		{name: "default quarter of memory", wantSize: 256 << 20},
		{name: "explicitly disabled", noBalloon: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := &hypervisor.VMRecord{Config: types.VMConfig{Memory: 1 << 30, NoBalloon: tt.noBalloon}}
			got := buildVMConfig(rec, "", nil).Balloon
			if tt.wantSize == 0 {
				if got != nil {
					t.Fatalf("balloon = %+v, want none", got)
				}
				return
			}
			if got == nil {
				t.Fatal("balloon device missing")
			}
			if got.Size != tt.wantSize {
				t.Errorf("balloon size = %d, want %d", got.Size, tt.wantSize)
			}
		})
	}
}

func TestCmdlineFollowsTheLiveNICs(t *testing.T) {
	rec := &hypervisor.VMRecord{
		Config: types.VMConfig{Name: "vm1", CPU: 2, Memory: 1 << 30},
		StorageConfigs: []*types.StorageConfig{
			{Path: "/run/layer0.erofs", RO: true, Role: types.StorageRoleLayer, Serial: "l0"},
			{Path: "/run/cow.raw", Role: types.StorageRoleCOW, Serial: hypervisor.CowSerial},
		},
		NetworkConfigs: []*types.NetworkConfig{
			{TAP: "tapvm1-0", MAC: "9a:29:2c:4b:27:e4", Network: &types.Network{IP: "10.211.0.134", Gateway: "10.211.0.1", Prefix: 22}},
		},
		BootConfig: &types.BootConfig{
			KernelPath: "/run/vmlinuz",
			InitrdPath: "/run/initrd.img",
			Cmdline:    "console=hvc0 loglevel=3 boot=cocoon-overlay cocoon.layers=l0 cocoon.cow=cow clocksource=kvm-clock rw net.ifnames=0 cocoon.hostname=vm1 ip=10.211.0.20::10.211.0.1:255.255.252.0:vm1:eth0:off",
		},
	}

	cfg := buildVMConfig(rec, "", nil)

	if cfg.Payload == nil {
		t.Fatal("no payload built for a direct-boot record")
	}
	if !strings.Contains(cfg.Payload.Cmdline, "ip=10.211.0.134::10.211.0.1:255.255.252.0:vm1:eth0:off") {
		t.Errorf("cmdline = %q, want the address the record now holds", cfg.Payload.Cmdline)
	}
	if strings.Contains(cfg.Payload.Cmdline, "10.211.0.20") {
		t.Errorf("cmdline = %q, still replays the address the VM was created with", cfg.Payload.Cmdline)
	}
}

func TestPayloadFollowsBootShape(t *testing.T) {
	storageConfigs := []*types.StorageConfig{
		{Path: "/run/layer0.erofs", RO: true, Role: types.StorageRoleLayer, Serial: "l0"},
		{Path: "/run/cow.raw", Role: types.StorageRoleCOW, Serial: hypervisor.CowSerial},
	}
	cmdline := buildCmdline(storageConfigs, nil, "vm1", nil)
	tests := []struct {
		name        string
		boot        types.BootConfig
		wantPayload chPayload
		wantArgs    []string
	}{
		{
			name:        "kernel and firmware boot through fw_cfg",
			boot:        types.BootConfig{KernelPath: "/boot/Image", InitrdPath: "/boot/initrd.img", FirmwarePath: "/fw/CLOUDHV.fd"},
			wantPayload: chPayload{Firmware: "/fw/CLOUDHV.fd", Kernel: "/boot/Image", Initramfs: "/boot/initrd.img", Cmdline: cmdline},
			wantArgs: []string{
				"--kernel", "/boot/Image",
				"--firmware", "/fw/CLOUDHV.fd",
				"--initramfs", "/boot/initrd.img",
				"--cmdline", cmdline,
				"--fw-cfg-config", "kernel=on,cmdline=on,initramfs=on,acpi_table=on",
			},
		},
		{
			name:        "kernel only boots directly",
			boot:        types.BootConfig{KernelPath: "/boot/vmlinuz", InitrdPath: "/boot/initrd.img"},
			wantPayload: chPayload{Kernel: "/boot/vmlinuz", Initramfs: "/boot/initrd.img", Cmdline: cmdline},
			wantArgs:    []string{"--kernel", "/boot/vmlinuz", "--initramfs", "/boot/initrd.img", "--cmdline", cmdline},
		},
		{
			name:        "firmware only boots UEFI",
			boot:        types.BootConfig{FirmwarePath: "/fw/CLOUDHV.fd"},
			wantPayload: chPayload{Firmware: "/fw/CLOUDHV.fd"},
			wantArgs:    []string{"--firmware", "/fw/CLOUDHV.fd"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &hypervisor.VMRecord{
				Config:         types.VMConfig{Name: "vm1", CPU: 1, Memory: 1 << 30},
				StorageConfigs: storageConfigs,
				BootConfig:     &tt.boot,
			}
			cfg := buildVMConfig(rec, "", nil)
			if cfg.Payload == nil || *cfg.Payload != tt.wantPayload {
				t.Fatalf("payload = %+v, want %+v", cfg.Payload, tt.wantPayload)
			}
			if got := payloadArgs(buildCLIArgs(cfg, "api.sock")); !slices.Equal(got, tt.wantArgs) {
				t.Errorf("payload args = %q, want %q", got, tt.wantArgs)
			}
		})
	}
}

func payloadArgs(args []string) []string {
	var out []string
	for i, a := range args {
		switch a {
		case "--firmware", "--kernel", "--initramfs", "--cmdline", "--fw-cfg-config":
			out = append(out, a, args[i+1])
		}
	}
	return out
}
