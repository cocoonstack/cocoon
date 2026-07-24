package daemon

import (
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestCachePublishesAddedThenModified(t *testing.T) {
	c := newCache()
	events, release := c.subscribe()
	defer release()

	c.publish([]VMStatus{statusOf("vm1", types.VMStateRunning, 1, true)}, true, 0, time.Now())
	if got := collect(t, events); len(got) != 1 || got[0].Kind != changeAdded {
		t.Fatalf("got %+v, want one ADDED", got)
	}

	c.publish([]VMStatus{statusOf("vm1", types.VMStateStopped, 2, false)}, true, 0, time.Now())
	got := collect(t, events)
	if len(got) != 1 || got[0].Kind != changeModified {
		t.Fatalf("got %+v, want one MODIFIED", got)
	}
	if got[0].Status.TransitionGeneration != 2 {
		t.Errorf("got generation %d, want 2", got[0].Status.TransitionGeneration)
	}
}

func TestCacheEmitsNothingForAnUnchangedPass(t *testing.T) {
	c := newCache()
	st := statusOf("vm1", types.VMStateRunning, 1, true)
	c.publish([]VMStatus{st}, true, 0, time.Now())
	events, release := c.subscribe()
	defer release()

	c.publish([]VMStatus{st}, true, 0, time.Now())
	if got := collect(t, events); len(got) != 0 {
		t.Errorf("got %+v, want no events when nothing a consumer acts on changed", got)
	}
}

func TestCachePublishesDeleted(t *testing.T) {
	c := newCache()
	c.publish([]VMStatus{statusOf("vm1", types.VMStateRunning, 1, true)}, true, 0, time.Now())
	events, release := c.subscribe()
	defer release()

	c.publish(nil, true, 0, time.Now())
	got := collect(t, events)
	if len(got) != 1 || got[0].Kind != changeDeleted || got[0].Status.ID != "vm1" {
		t.Fatalf("got %+v, want one DELETED for vm1", got)
	}
}

func TestCacheSnapshotCarriesHealth(t *testing.T) {
	c := newCache()
	at := time.Now()
	c.publish([]VMStatus{statusOf("vm1", types.VMStateRunning, 1, true)}, false, 2, at)

	all, h := c.snapshot()
	if len(all) != 1 {
		t.Fatalf("got %d statuses, want 1", len(all))
	}
	if h.ok {
		t.Error("a pass that failed a backend scan reported healthy")
	}
	if h.degraded != 2 {
		t.Errorf("got degraded %d, want 2", h.degraded)
	}
	if !h.lastPass.Equal(at) {
		t.Errorf("got last pass %v, want %v", h.lastPass, at)
	}
}

// A released subscription must not keep receiving, and must not block later passes.
func TestCacheReleaseStopsDelivery(t *testing.T) {
	c := newCache()
	events, release := c.subscribe()
	release()

	c.publish([]VMStatus{statusOf("vm1", types.VMStateRunning, 1, true)}, true, 0, time.Now())
	if got := collect(t, events); len(got) != 0 {
		t.Errorf("got %+v, want nothing after release", got)
	}
}

func statusOf(id string, state types.VMState, gen uint64, live bool) VMStatus {
	rec := &hypervisor.VMRecord{VM: types.VM{ID: id, State: state, TransitionGeneration: gen}}
	return newVMStatus("fake-hv", rec, live, 0, time.Now())
}

func collect(t *testing.T, ch <-chan changeEvent) []changeEvent {
	t.Helper()
	var got []changeEvent
	for {
		select {
		case ev := <-ch:
			got = append(got, ev)
		default:
			return got
		}
	}
}
