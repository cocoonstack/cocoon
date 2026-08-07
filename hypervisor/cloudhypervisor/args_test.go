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
