package utils

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitFor_ImmediateSuccess(t *testing.T) {
	err := WaitFor(t.Context(), time.Second, 10*time.Millisecond, func() (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitFor_EventualSuccess(t *testing.T) {
	calls := 0
	err := WaitFor(t.Context(), time.Second, 10*time.Millisecond, func() (bool, error) {
		calls++
		return calls >= 3, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls < 3 {
		t.Errorf("expected at least 3 calls, got %d", calls)
	}
}

func TestWaitFor_Timeout(t *testing.T) {
	err := WaitFor(t.Context(), 100*time.Millisecond, 10*time.Millisecond, func() (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

func TestWaitFor_CheckError(t *testing.T) {
	sentinel := errors.New("check failed")
	err := WaitFor(t.Context(), time.Second, 10*time.Millisecond, func() (bool, error) {
		return false, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got: %v", err)
	}
}

func TestWaitFor_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := WaitFor(ctx, time.Second, 10*time.Millisecond, func() (bool, error) {
		return false, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestWaitFor_ContextDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := WaitFor(ctx, 10*time.Second, 10*time.Millisecond, func() (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
}
