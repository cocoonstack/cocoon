package hypervisor

import (
	"errors"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/types"
)

func TestLockedRunningOpRefusesAVMThatStoppedWhileItWaited(t *testing.T) {
	b, id := newHibernateTestVM(t)
	ctx := t.Context()
	unlock, err := b.LockVMOps(ctx, id)
	if err != nil {
		t.Fatalf("LockVMOps: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, release, opErr := b.LockedRunningOp(ctx, id)
		if opErr == nil {
			release()
		}
		done <- opErr
	}()
	time.Sleep(200 * time.Millisecond)
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateStopped
		return nil
	}); err != nil {
		t.Fatalf("flip state: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("LockedRunningOp returned %v before the lock was released, want it blocked on the lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-done; !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}
