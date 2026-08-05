package cloudhypervisor

import (
	"slices"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestQueueAffinityClamp(t *testing.T) {
	tests := []struct {
		name    string
		cpu     int
		allowed []int
		want    [][]int
	}{
		{name: "identity without allowance", cpu: 3, want: [][]int{{0}, {1}, {2}}},
		{name: "clamped round-robin", cpu: 4, allowed: []int{8, 9}, want: [][]int{{8}, {9}, {8}, {9}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qa := queueAffinity(tt.cpu, tt.allowed)
			for i, a := range qa {
				if a.QueueIndex != i || !slices.Equal(a.HostCPUs, tt.want[i]) {
					t.Errorf("queue %d: got %v, want %v", i, a.HostCPUs, tt.want[i])
				}
			}
		})
	}
}

func TestHostCPUAllowancePlacementWinsOverFence(t *testing.T) {
	cfg := &types.Config{CPUSetCPUs: "2-3"}
	if got := hostCPUAllowance(cfg, "0-14"); !slices.Equal(got, []int{2, 3}) {
		t.Errorf("placement: got %v", got)
	}
	if got := hostCPUAllowance(&types.Config{}, "0-1"); !slices.Equal(got, []int{0, 1}) {
		t.Errorf("fence fallback: got %v", got)
	}
	if got := hostCPUAllowance(&types.Config{}, ""); got != nil {
		t.Errorf("no fence: got %v, want nil", got)
	}
}
