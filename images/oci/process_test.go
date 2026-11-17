package oci

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryLayer(t *testing.T) {
	sentinel := errors.New("stream broke")

	tests := []struct {
		name      string
		failFirst int
		wantErr   bool
		wantCalls int
	}{
		{"succeeds after transient failures", 2, false, 3},
		{"exhausts attempts and returns the last error", -1, true, 3},
		{"first success needs one call", 0, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			err := retryLayer(t.Context(), 3, time.Millisecond, func() error {
				calls++
				if tt.failFirst < 0 || calls <= tt.failFirst {
					return sentinel
				}
				return nil
			})
			if tt.wantErr && !errors.Is(err, sentinel) {
				t.Fatalf("retryLayer() = %v, want %v", err, sentinel)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("retryLayer() = %v, want nil", err)
			}
			if calls != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}

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
