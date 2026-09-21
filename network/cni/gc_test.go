package cni

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cocoonstack/cocoon/lock/vmlock"
)

func TestGCRechecksOwnerUnderVMLock(t *testing.T) {
	readErr := errors.New("owner read failed")
	for _, tt := range []struct {
		name  string
		inUse bool
		err   error
		busy  bool
	}{
		{name: "completed create", inUse: true},
		{name: "orphan"},
		{name: "read failure", err: readErr},
		{name: "create in flight", busy: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, exec := newTestCNIWithStore(t)
			stubLifecycleSeams(t)
			ctx := t.Context()
			seedRecords(t, c, "vm1", "eth0")
			lk, err := vmlock.New(c.conf.RootDir, "vm1")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lk.Unlock(ctx) })
			calls := 0
			m := c.GCModule(func(ctx context.Context, id string) (bool, error) {
				calls++
				if id != "vm1" {
					t.Fatalf("owner ref = %q, want vm1", id)
				}
				if ok, err := lk.TryLock(ctx); err != nil || ok {
					t.Fatalf("owner read must hold VM lock: acquired=%v err=%v", ok, err)
				}
				return tt.inUse, tt.err
			})
			snap, err := m.ReadDB(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ids := m.Resolve(ctx, snap, nil)
			if !slices.Contains(ids, "vm1") {
				t.Fatalf("stale candidates = %v", ids)
			}
			if tt.busy {
				if err := lk.Lock(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Collect(ctx, []string{"vm1"}, snap); !errors.Is(err, tt.err) {
				t.Fatalf("collect error = %v, want %v", err, tt.err)
			}
			if (calls == 0) != tt.busy {
				t.Errorf("owner reads = %d, busy=%v", calls, tt.busy)
			}
			if tt.inUse || tt.err != nil || tt.busy {
				assertRecordIDs(t, c, []string{"n-eth0"})
				if len(exec.attempted) != 0 {
					t.Errorf("CNI DEL attempted: %v", exec.attempted)
				}
			} else {
				assertRecordIDs(t, c, nil)
				if !slices.Contains(exec.attempted, "eth0") {
					t.Errorf("CNI DEL not attempted: %v", exec.attempted)
				}
			}
		})
	}
}
