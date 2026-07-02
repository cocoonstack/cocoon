package storage

import (
	"context"
)

// Initer is optionally implemented by T to initialize zero-value fields (e.g., nil maps) after deserialization or when the backing store is empty.
type Initer interface {
	Init()
}

// Store provides locked read/modify/write access to a data store of top-level type T.
type Store[T any] interface {
	// With loads under lock and passes to fn; *T's Init() runs first if implemented; lock held for fn's duration.
	With(ctx context.Context, fn func(*T) error) error
	// Update performs a read-modify-write under lock; persists only if fn returns nil.
	Update(ctx context.Context, fn func(*T) error) error

	// ReadRaw deserializes and passes to fn without locking; caller must already hold the lock via TryLock.
	ReadRaw(fn func(*T) error) error
	// WriteRaw deserializes, runs fn, atomically persists; caller must hold the lock (via TryLock).
	WriteRaw(fn func(*T) error) error
	// TryLock non-blocking acquire; (false, nil) if held; on success caller must Unlock.
	TryLock(ctx context.Context) (bool, error)
	// Unlock releases a lock previously acquired by TryLock.
	Unlock(ctx context.Context) error
}
