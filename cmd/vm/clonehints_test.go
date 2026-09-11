package vm

import (
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestPostCloneHintsGateBalloonRelease(t *testing.T) {
	for _, tt := range []struct {
		name      string
		memory    int64
		noBalloon bool
		want      bool
	}{
		{name: "balloon present", memory: 1 << 30, want: true},
		{name: "opted out", memory: 1 << 30, noBalloon: true},
		{name: "below the balloon floor", memory: 128 << 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			vm := &types.VM{Hypervisor: "cloud-hypervisor", Config: types.VMConfig{
				Name:   "c",
				Config: types.Config{Memory: tt.memory, NoBalloon: tt.noBalloon, ImageType: types.ImageTypeOCI},
			}}
			out := captureStdout(t, func() { printPostCloneHints(vm) })
			if got := strings.Contains(out, "drop_caches"); got != tt.want {
				t.Errorf("drop_caches hint = %v, want %v (output: %q)", got, tt.want, out)
			}
		})
	}
}
