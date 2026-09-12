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
		want      []chQueueAffinity
	}{
		{name: "no placement leaves the queues unpinned", cpu: 3},
		{name: "a single queue is never pinned", cpu: 1, placement: []int{8, 9}},
		{name: "round-robin within the placement", cpu: 4, placement: []int{8, 9}, want: []chQueueAffinity{
			{QueueIndex: 0, HostCPUs: []int{8}},
			{QueueIndex: 1, HostCPUs: []int{9}},
			{QueueIndex: 2, HostCPUs: []int{8}},
			{QueueIndex: 3, HostCPUs: []int{9}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := queueAffinity(tt.cpu, tt.placement); !slices.EqualFunc(got, tt.want, sameQueueAffinity) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDiskCLIArgQueueAffinity(t *testing.T) {
	tests := []struct {
		name      string
		placement []int
		want      string
	}{
		{name: "unplaced VMs must not pin queue threads onto the low core ids every other VM also picks", placement: nil},
		{name: "a placement renders one host cpu per queue", placement: []int{8, 9}, want: "queue_affinity=[0@[8],1@[9],2@[8],3@[9]]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &types.StorageConfig{Path: "/v/cow.raw", Role: types.StorageRoleCOW}
			got := diskToCLIArg(storageConfigToDisk(sc, 4, 0, false, tt.placement))
			if tt.want == "" {
				if strings.Contains(got, "queue_affinity") {
					t.Errorf("got %s, want no queue_affinity", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("got %s, want it to contain %s", got, tt.want)
			}
		})
	}
}
