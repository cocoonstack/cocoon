// Package gc runs modular garbage collection: each subsystem registers a
// Module; recovery precedes discovery (design §5), snapshots are loose, and
// every destructive decision is revalidated by the module under its entity
// locks and tombstone leases — the orchestrator holds no locks of its own.
package gc

import (
	"context"
)

// Module[S] is a typed GC participant; S is the snapshot type ReadDB returns and Resolve consumes.
type Module[S any] struct {
	Name string

	// Recover resumes existing tombstones by phase BEFORE discovery: a
	// deleting entry whose data is already gone never reappears as a
	// candidate, and stranding it would leak forever. Optional.
	Recover func(ctx context.Context) []error

	// ReadDB reads the module's current state (self-locking snapshot).
	ReadDB func(ctx context.Context) (S, error)

	// Resolve returns IDs to delete; others holds snapshots from peer modules (cross-module analysis, e.g. VMs pinning images). Loose: collectors revalidate per candidate.
	Resolve func(ctx context.Context, snap S, others map[string]any) []string

	// Collect removes the given IDs, revalidating each under its entity lock and tombstone lease.
	Collect func(ctx context.Context, ids []string, snap S) error
}

// Module[S] implements runner.
func (m Module[S]) getName() string { return m.Name }

func (m Module[S]) recover(ctx context.Context) []error {
	if m.Recover == nil {
		return nil
	}
	return m.Recover(ctx)
}

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
