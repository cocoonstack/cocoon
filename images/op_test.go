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
	// The blocking flight must register first, or the canceled call could win
	// the key and select its own instant result over the canceled ctx.
	<-started

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := SingleflightDo(ctx, &g, "k", func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled waiter err = %v, want context.Canceled", err)
	}
	// The shared work must still be running — unblock it so the goroutine exits.
	close(block)
	<-done
}
