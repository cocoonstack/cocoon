package lock

import "context"

// Locker provides mutual exclusion with context support.
type Locker interface {
	Lock(ctx context.Context) error
	Unlock(ctx context.Context) error
	// TryLock returns (false, nil) when the lock is already held by another caller.
	TryLock(ctx context.Context) (bool, error)
}
