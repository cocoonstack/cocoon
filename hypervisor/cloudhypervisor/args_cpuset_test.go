package cloudhypervisor

import (
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestQueueAffinityFollowsQueueCPUs(t *testing.T) {
	tests := []struct {
		name      string
		cpu       int
		queueCPUs []int
		want      []chQueueAffinity
	}{
		{name: "no pins leave the queues unpinned", cpu: 3},
		{name: "a single queue is never pinned", cpu: 1, queueCPUs: []int{8, 9}},
		{name: "round-robin over the pins", cpu: 4, queueCPUs: []int{8, 9}, want: []chQueueAffinity{
			{QueueIndex: 0, HostCPUs: []int{8}},
			{QueueIndex: 1, HostCPUs: []int{9}},
			{QueueIndex: 2, HostCPUs: []int{8}},
			{QueueIndex: 3, HostCPUs: []int{9}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := queueAffinity(tt.cpu, tt.queueCPUs); !slices.EqualFunc(got, tt.want, sameQueueAffinity) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDiskCLIArgQueueAffinity(t *testing.T) {
	tests := []struct {
		name      string
		queueCPUs []int
		want      string
	}{
		{name: "no pins render no affinity", queueCPUs: nil},
		{name: "pins render one host cpu per queue", queueCPUs: []int{8, 9}, want: "queue_affinity=[0@[8],1@[9],2@[8],3@[9]]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &types.StorageConfig{Path: "/v/cow.raw", Role: types.StorageRoleCOW}
			got := diskToCLIArg(storageConfigToDisk(sc, 4, 0, false, tt.queueCPUs))
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
