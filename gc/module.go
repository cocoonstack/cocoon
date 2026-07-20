// Package gc runs modular garbage collection: recovery precedes discovery, snapshots are loose, and every destructive decision is revalidated by the module under its own entity locks and tombstone leases.
package gc

import (
	"context"
)

// Module[S] is a typed GC participant; S is the snapshot type ReadDB returns and Resolve consumes.
type Module[S any] struct {
	Name string

	// Recover resumes existing tombstones by phase before discovery, so a deleting entry whose data is already gone never reappears as a candidate. Optional.
	Recover func(ctx context.Context) []error

	// ReadDB reads the module's current state (self-locking snapshot).
	ReadDB func(ctx context.Context) (S, error)

	// Resolve returns IDs to delete; others holds snapshots from peer modules (cross-module analysis, e.g. VMs pinning images). Loose: collectors revalidate per candidate.
	Resolve func(ctx context.Context, snap S, others map[string]any) []string

	// Collect removes the given IDs, revalidating each under its entity lock and tombstone lease.
	Collect func(ctx context.Context, ids []string, snap S) error
}

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
