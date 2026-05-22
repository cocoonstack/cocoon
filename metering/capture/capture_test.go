package capture

import (
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/metering"
)

func TestRecorderBasic(t *testing.T) {
	r := New()
	ctx := t.Context()
	r.Emit(ctx, metering.Entry{Kind: metering.KindVMComputeStart, VMID: "a"})
	r.Emit(ctx, metering.Entry{Kind: metering.KindVMComputeStop, VMID: "a"})
	got := r.Entries()
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Kind != metering.KindVMComputeStart || got[1].Kind != metering.KindVMComputeStop {
		t.Errorf("got kinds %v %v", got[0].Kind, got[1].Kind)
	}
}

func TestRecorderEntriesIsCopy(t *testing.T) {
	r := New()
	r.Emit(t.Context(), metering.Entry{VMID: "a"})
	got := r.Entries()
	got[0].VMID = "tampered"
	if again := r.Entries(); again[0].VMID != "a" {
		t.Errorf("Entries() must return a copy; got %q after mutation", again[0].VMID)
	}
}

func TestRecorderReset(t *testing.T) {
	r := New()
	r.Emit(t.Context(), metering.Entry{VMID: "a"})
	r.Reset()
	if got := r.Entries(); len(got) != 0 {
		t.Errorf("after Reset got %d entries, want 0", len(got))
	}
}

func TestRecorderConcurrent(t *testing.T) {
	r := New()
	ctx := t.Context()
	const N = 200
	var wg sync.WaitGroup
	for range N {
		wg.Go(func() {
			r.Emit(ctx, metering.Entry{Kind: metering.KindVMComputeStart, VMID: "vm"})
		})
	}
	wg.Wait()
	if got := len(r.Entries()); got != N {
		t.Errorf("got %d entries, want %d", got, N)
	}
}
