package oci

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryLayer(t *testing.T) {
	sentinel := errors.New("stream broke")

	t.Run("succeeds after transient failures", func(t *testing.T) {
		calls := 0
		err := retryLayer(t.Context(), 3, time.Millisecond, func() error {
			calls++
			if calls < 3 {
				return sentinel
			}
			return nil
		})
		if err != nil {
			t.Fatalf("retryLayer() = %v, want nil", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})

	t.Run("exhausts attempts and returns the last error", func(t *testing.T) {
		calls := 0
		err := retryLayer(t.Context(), 3, time.Millisecond, func() error {
			calls++
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("retryLayer() = %v, want %v", err, sentinel)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})

	t.Run("first success needs one call", func(t *testing.T) {
		calls := 0
		if err := retryLayer(t.Context(), 3, time.Millisecond, func() error {
			calls++
			return nil
		}); err != nil {
			t.Fatalf("retryLayer() = %v, want nil", err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
	})

	t.Run("canceled context stops the retries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		err := retryLayer(ctx, 5, time.Hour, func() error {
			calls++
			cancel()
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("retryLayer() = %v, want %v", err, sentinel)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (no retry after cancel)", calls)
		}
	})
}
