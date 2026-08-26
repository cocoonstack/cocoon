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
			rec := &hypervisor.VMRecord{VM: types.VM{Config: types.VMConfig{Config: tt.cfg}}}
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
		{name: "explicit override wins", sc: types.StorageConfig{Path: "/v/data.raw", Role: types.StorageRoleData, DirectIO: ptr(false)}},
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
			rec := &hypervisor.VMRecord{VM: types.VM{Config: types.VMConfig{Config: types.Config{NoWatchdog: tt.noWatchdog}}}}
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

func ptr[T any](v T) *T { return &v }
