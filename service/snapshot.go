package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/types"
)

// SaveSnapshot snapshots a running VM and stores the result.
func (s *Service) SaveSnapshot(ctx context.Context, p *SnapshotSaveParams) (string, error) {
	// Pre-check: reject if the snapshot name is already taken.
	if p.Name != "" {
		if _, inspectErr := s.snapshot.Inspect(ctx, p.Name); inspectErr == nil {
			return "", fmt.Errorf("snapshot name %q already exists", p.Name)
		} else if !errors.Is(inspectErr, snapshot.ErrNotFound) {
			return "", fmt.Errorf("check snapshot name: %w", inspectErr)
		}
	}

	// Snapshot the VM.
	cfg, stream, err := s.hypervisor.Snapshot(ctx, p.VMRef)
	if err != nil {
		return "", fmt.Errorf("snapshot VM %s: %w", p.VMRef, err)
	}
	defer stream.Close() //nolint:errcheck

	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	cfg.Name = p.Name
	cfg.Description = p.Description

	// Store the snapshot data.
	snapID, err := s.snapshot.Create(ctx, cfg, stream)
	if err != nil {
		return "", fmt.Errorf("save snapshot: %w", err)
	}

	return snapID, nil
}

// ListSnapshots returns all snapshots, optionally filtered by VM.
func (s *Service) ListSnapshots(ctx context.Context, vmRef string) ([]*types.Snapshot, error) {
	// Optional: filter by VM ownership.
	var filterIDs map[string]struct{}

	if vmRef != "" {
		vm, err := s.hypervisor.Inspect(ctx, vmRef)
		if err != nil {
			return nil, fmt.Errorf("inspect VM %s: %w", vmRef, err)
		}

		filterIDs = vm.SnapshotIDs
		if len(filterIDs) == 0 {
			return nil, nil
		}
	}

	snapshots, err := s.snapshot.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}

	// Apply VM filter if specified.
	if filterIDs != nil {
		filtered := snapshots[:0]

		for _, snap := range snapshots {
			if _, ok := filterIDs[snap.ID]; ok {
				filtered = append(filtered, snap)
			}
		}

		snapshots = filtered
	}

	slices.SortFunc(snapshots, func(a, b *types.Snapshot) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	return snapshots, nil
}

// InspectSnapshot returns a snapshot by reference.
func (s *Service) InspectSnapshot(ctx context.Context, ref string) (*types.Snapshot, error) {
	snap, err := s.snapshot.Inspect(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}

	return snap, nil
}

// RemoveSnapshots deletes snapshots by reference.
func (s *Service) RemoveSnapshots(ctx context.Context, refs []string) ([]string, error) {
	deleted, err := s.snapshot.Delete(ctx, refs)
	if err != nil {
		return deleted, fmt.Errorf("rm: %w", err)
	}

	return deleted, nil
}
