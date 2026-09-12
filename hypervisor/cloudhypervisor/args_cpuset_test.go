package cloudhypervisor

import (
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestQueueAffinityFollowsPlacement(t *testing.T) {
	tests := []struct {
		name      string
		cpu       int
		placement []int
		want      [][]int
	}{
		{name: "no placement leaves the queues unpinned", cpu: 3},
		{name: "round-robin within the placement", cpu: 4, placement: []int{8, 9}, want: [][]int{{8}, {9}, {8}, {9}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qa := queueAffinity(tt.cpu, tt.placement)
			if len(qa) != len(tt.want) {
				t.Fatalf("got %d entries, want %d", len(qa), len(tt.want))
			}
			for i, a := range qa {
				if a.QueueIndex != i || !slices.Equal(a.HostCPUs, tt.want[i]) {
					t.Errorf("queue %d: got %v, want %v", i, a.HostCPUs, tt.want[i])
				}
			}
		})
	}
}

func TestDiskCLIArgOmitsQueueAffinityWithoutPlacement(t *testing.T) {
	sc := &types.StorageConfig{Path: "/v/cow.raw", Role: types.StorageRoleCOW}
	got := diskToCLIArg(storageConfigToDisk(sc, 8, 0, false, nil))
	if strings.Contains(got, "queue_affinity") {
		t.Errorf("unplaced VMs must not pin queue threads onto the low core ids every other VM also picks: %s", got)
	}
}
