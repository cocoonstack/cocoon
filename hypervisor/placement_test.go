package hypervisor

import (
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestPlaceRecord(t *testing.T) {
	type other struct {
		state     types.VMState
		queueCPUs string
	}
	tests := []struct {
		name          string
		cfg           types.Config
		noQueuePins   bool
		selfState     types.VMState
		selfQueueCPUs string
		others        []other
		wantCPUSet    string
		wantQueueCPUs string
	}{
		{name: "explicit cpuset is copied to both", cfg: types.Config{CPU: 4, CPUSetCPUs: "2-3"}, wantCPUSet: "2-3", wantQueueCPUs: "2-3"},
		{name: "a backend without queue pins copies only the cpuset", cfg: types.Config{CPU: 4, CPUSetCPUs: "2-3"}, noQueuePins: true, wantCPUSet: "2-3"},
		{name: "a backend without queue pins still places auto", cfg: types.Config{CPU: 4, CPUSetCPUs: "auto"}, noQueuePins: true, wantCPUSet: "0-7"},
		{name: "a backend without queue pins records nothing by default", cfg: types.Config{CPU: 4}, noQueuePins: true},
		{name: "no cpuset pins one thread per queue on an empty host", cfg: types.Config{CPU: 4}, wantQueueCPUs: "0-3"},
		{name: "live pins push the next VM onto idle cpus", cfg: types.Config{CPU: 4}, others: []other{{types.VMStateRunning, "0-3"}, {types.VMStateCreating, "4-5"}}, wantQueueCPUs: "0-1,6-7"},
		{name: "stopped VMs hold no pins", cfg: types.Config{CPU: 4}, others: []other{{types.VMStateStopped, "0-3"}}, wantQueueCPUs: "0-3"},
		{name: "a VM's own stale pins are not load", cfg: types.Config{CPU: 4}, selfState: types.VMStateRunning, selfQueueCPUs: "0-3", wantQueueCPUs: "0-3"},
		{name: "auto places the cache domain and pins inside it", cfg: types.Config{CPU: 2, CPUSetCPUs: "auto"}, wantCPUSet: "0-7", wantQueueCPUs: "0-1"},
		{name: "a single queue is never pinned", cfg: types.Config{CPU: 1}},
		{name: "auto with a single queue still places the domain", cfg: types.Config{CPU: 1, CPUSetCPUs: "auto"}, wantCPUSet: "0-7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newMeteringTestBackend(t)
			b.PinsQueues = !tt.noQueuePins
			ctx := t.Context()
			if err := b.update(ctx, func(tx *vmTx) error {
				if err := tx.Put("self", &VMRecord{VM: types.VM{ID: "self", State: tt.selfState, QueueCPUs: tt.selfQueueCPUs}}); err != nil {
					return err
				}
				for i, o := range tt.others {
					id := string(rune('a' + i))
					if err := tx.Put(id, &VMRecord{VM: types.VM{ID: id, State: o.state, QueueCPUs: o.queueCPUs}}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			placed, err := b.placeRecord(ctx, "self", &tt.cfg)
			if err != nil {
				t.Fatalf("placeRecord: %v", err)
			}
			if placed.CPUSet != tt.wantCPUSet || placed.QueueCPUs != tt.wantQueueCPUs {
				t.Errorf("got cpuset %q queue_cpus %q, want %q %q", placed.CPUSet, placed.QueueCPUs, tt.wantCPUSet, tt.wantQueueCPUs)
			}
			stored, err := b.PeekRecord(ctx, "self")
			if err != nil {
				t.Fatalf("PeekRecord: %v", err)
			}
			if stored.CPUSet != placed.CPUSet || stored.QueueCPUs != placed.QueueCPUs {
				t.Errorf("stored cpuset %q queue_cpus %q differ from placed", stored.CPUSet, stored.QueueCPUs)
			}
		})
	}
}
