package network

import (
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestAddRecover(t *testing.T) {
	existing := []*types.NetworkConfig{
		{TAP: "tap0", NumQueues: 2},
		nil, // persisted network_configs may carry null entries
		{TAP: "tap2", NumQueues: 4},
	}
	specs := AddRecover(existing)
	if len(specs) != 3 {
		t.Fatalf("got %d specs, want 3", len(specs))
	}
	if specs[0].Queues != 2 || specs[2].Queues != 4 {
		t.Errorf("persisted queue counts not restored: %+v", specs)
	}
	if specs[1].Queues != 0 || specs[1].Existing != nil {
		t.Errorf("nil NIC must keep the derive-from-CPU default: %+v", specs[1])
	}
	if specs[1].Index != 1 {
		t.Errorf("index not preserved for nil entry: %+v", specs[1])
	}
}

func TestAddRange(t *testing.T) {
	specs := AddRange(2, 3)
	if len(specs) != 3 || specs[0].Index != 2 || specs[2].Index != 4 {
		t.Fatalf("got %+v", specs)
	}
	for _, s := range specs {
		if s.Queues != 0 || s.Existing != nil {
			t.Errorf("fresh spec must be zero beyond Index: %+v", s)
		}
	}
}
