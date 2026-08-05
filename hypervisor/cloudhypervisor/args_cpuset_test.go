package cloudhypervisor

import (
	"slices"
	"testing"
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
