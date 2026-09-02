package images

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sync/singleflight"
)

func TestSingleflightDoDetachesCanceledWaiter(t *testing.T) {
	var g singleflight.Group
	block := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_ = SingleflightDo(t.Context(), &g, "k", func() error {
			close(started)
			<-block
			return nil
		})
		close(done)
	}()

	<-started

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := SingleflightDo(ctx, &g, "k", func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled waiter err = %v, want context.Canceled", err)
	}

	close(block)
	<-done
}
