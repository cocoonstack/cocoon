// Package gc runs modular, lock-safe garbage collection: each subsystem
// registers a Module and cross-module reference protocols keep live
// resources pinned.
package gc

import (
	"context"

	"github.com/cocoonstack/cocoon/lock"
)

// Module[S] is a typed GC participant; S is the snapshot type ReadDB returns and Resolve consumes.
type Module[S any] struct {
	Name   string
	Locker lock.Locker

	// ReadDB reads the module's current state (called while the lock is held).
	ReadDB func(ctx context.Context) (S, error)

	// Resolve returns IDs to delete; others holds snapshots from peer modules (cast for cross-module analysis, e.g. VMs pinning images).
	Resolve func(ctx context.Context, snap S, others map[string]any) []string

	// Collect removes the given IDs (called while the lock is held).
	Collect func(ctx context.Context, ids []string, snap S) error
}

// Module[S] implements runner.
func (m Module[S]) getName() string        { return m.Name }
func (m Module[S]) getLocker() lock.Locker { return m.Locker }

func (m Module[S]) readSnapshot(ctx context.Context) (any, error) {
	return m.ReadDB(ctx)
}

func (m Module[S]) resolveTargets(ctx context.Context, snap any, others map[string]any) []string {
	typed, ok := snap.(S)
	if !ok {
		return nil
	}
	return m.Resolve(ctx, typed, others)
}

func (m Module[S]) collect(ctx context.Context, ids []string, snap any) error {
	typed, ok := snap.(S)
	if !ok {
		return nil
	}
	return m.Collect(ctx, ids, typed)
}
