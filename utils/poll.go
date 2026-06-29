package utils

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// WaitFor polls check at the given interval until it returns (true, nil), returns a non-nil error, or the timeout/context expires.
func WaitFor(ctx context.Context, timeout, interval time.Duration, check func() (done bool, err error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timeout after %s", timeout)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
