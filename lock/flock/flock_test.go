package flock

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.lock")
}

func TestLockUnlock(t *testing.T) {
	l := New(lockPath(t))
	ctx := t.Context()

	if err := l.Lock(ctx); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestLockBlocksConcurrent(t *testing.T) {
	l := New(lockPath(t))
	ctx := t.Context()

	if err := l.Lock(ctx); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		if err := l.Lock(ctx); err != nil {
			t.Errorf("second Lock: %v", err)
			return
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second Lock should block while first is held")
	case <-time.After(200 * time.Millisecond):
	}

	if err := l.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("second Lock should have acquired after Unlock")
	}

	if err := l.Unlock(ctx); err != nil {
		t.Fatalf("cleanup Unlock: %v", err)
	}
}

func TestTryLockHeld(t *testing.T) {
	l := New(lockPath(t))
	ctx := t.Context()

	if err := l.Lock(ctx); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	got, err := l.TryLock(ctx)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if got {
		t.Fatal("TryLock should return false when lock is held in-process")
	}

	if err := l.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestTryLockCrossInstance(t *testing.T) {
	path := lockPath(t)
	l1 := New(path)
	l2 := New(path)
	ctx := t.Context()

	if err := l1.Lock(ctx); err != nil {
		t.Fatalf("l1 Lock: %v", err)
	}

	got, err := l2.TryLock(ctx)
	if err != nil {
		t.Fatalf("l2 TryLock: %v", err)
	}
	if got {
		t.Fatal("l2 TryLock should fail while l1 holds the flock")
	}

	if err := l1.Unlock(ctx); err != nil {
		t.Fatalf("l1 Unlock: %v", err)
	}

	got, err = l2.TryLock(ctx)
	if err != nil {
		t.Fatalf("l2 TryLock after release: %v", err)
	}
	if !got {
		t.Fatal("l2 TryLock should succeed after l1 releases")
	}

	if err := l2.Unlock(ctx); err != nil {
		t.Fatalf("l2 Unlock: %v", err)
	}
}

func TestLockContextCancel(t *testing.T) {
	l := New(lockPath(t))
	ctx := t.Context()

	if err := l.Lock(ctx); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	err := l.Lock(cancelCtx)
	if err == nil {
		t.Fatal("Lock with canceled context should return error")
	}

	if err := l.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestConcurrentLockUnlock(t *testing.T) {
	l := New(lockPath(t))
	ctx := t.Context()

	var (
		mu      sync.Mutex
		counter int
	)

	var wg sync.WaitGroup
	wg.Add(50)
	for range 50 {
		go func() {
			defer wg.Done()
			if err := l.Lock(ctx); err != nil {
				t.Errorf("Lock: %v", err)
				return
			}
			mu.Lock()
			counter++
			mu.Unlock()
			if err := l.Unlock(ctx); err != nil {
				t.Errorf("Unlock: %v", err)
			}
		}()
	}
	wg.Wait()

	if counter != 50 {
		t.Fatalf("counter = %d, want 50", counter)
	}
}

func TestUnlockWithoutLock(t *testing.T) {
	l := New(lockPath(t))
	ctx := t.Context()

	if err := l.Unlock(ctx); err != nil {
		t.Fatalf("Unlock on unheld lock should not error, got: %v", err)
	}
}
