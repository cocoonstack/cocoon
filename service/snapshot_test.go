package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/types"
)

func TestSaveSnapshot_Success(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.SnapshotFunc = func(_ context.Context, _ string) (*types.SnapshotConfig, io.ReadCloser, error) {
		cfg := &types.SnapshotConfig{ID: "snap-1", CPU: 2, Memory: 1 << 30}
		return cfg, io.NopCloser(strings.NewReader("data")), nil
	}

	snap := defaultSnapshot()
	snap.InspectFunc = func(_ context.Context, _ string) (*types.Snapshot, error) {
		return nil, snapshot.ErrNotFound
	}
	snap.CreateFunc = func(_ context.Context, cfg *types.SnapshotConfig, _ io.Reader) (string, error) {
		if cfg.Name != "my-snap" {
			t.Errorf("expected name 'my-snap', got %q", cfg.Name)
		}
		return "snap-001", nil
	}

	svc := newTestService(hyper, nil, nil, snap)

	id, err := svc.SaveSnapshot(context.Background(), &SnapshotSaveParams{
		VMRef: "vm-1",
		Name:  "my-snap",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id != "snap-001" {
		t.Errorf("expected snap-001, got %s", id)
	}
}

func TestSaveSnapshot_DuplicateName(t *testing.T) {
	snap := defaultSnapshot()
	snap.InspectFunc = func(_ context.Context, ref string) (*types.Snapshot, error) {
		if ref == "existing-name" {
			return &types.Snapshot{SnapshotConfig: types.SnapshotConfig{ID: "snap-old", Name: "existing-name"}}, nil
		}
		return nil, snapshot.ErrNotFound
	}

	svc := newTestService(defaultHypervisor(), nil, nil, snap)

	_, err := svc.SaveSnapshot(context.Background(), &SnapshotSaveParams{
		VMRef: "vm-1",
		Name:  "existing-name",
	})
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}

	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' in error, got: %v", err)
	}
}

func TestSaveSnapshot_VMSnapshotFailure(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.SnapshotFunc = func(_ context.Context, _ string) (*types.SnapshotConfig, io.ReadCloser, error) {
		return nil, nil, fmt.Errorf("VM is stopped")
	}

	snap := defaultSnapshot()
	snap.InspectFunc = func(_ context.Context, _ string) (*types.Snapshot, error) {
		return nil, snapshot.ErrNotFound
	}

	svc := newTestService(hyper, nil, nil, snap)

	_, err := svc.SaveSnapshot(context.Background(), &SnapshotSaveParams{
		VMRef: "vm-1",
		Name:  "new-snap",
	})
	if err == nil {
		t.Fatal("expected error")
	}

	if !strings.Contains(err.Error(), "snapshot VM") {
		t.Errorf("expected 'snapshot VM' in error, got: %v", err)
	}
}

func TestListSnapshots_Success(t *testing.T) {
	snap := defaultSnapshot()
	snap.ListFunc = func(_ context.Context) ([]*types.Snapshot, error) {
		return []*types.Snapshot{
			{SnapshotConfig: types.SnapshotConfig{ID: "snap-1", Name: "first"}, CreatedAt: time.Now()},
			{SnapshotConfig: types.SnapshotConfig{ID: "snap-2", Name: "second"}, CreatedAt: time.Now()},
		}, nil
	}

	svc := newTestService(defaultHypervisor(), nil, nil, snap)

	snaps, err := svc.ListSnapshots(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snaps) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(snaps))
	}
}

func TestListSnapshots_FilteredByVM(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.InspectFunc = func(_ context.Context, _ string) (*types.VM, error) {
		return &types.VM{
			ID:          "vm-1",
			SnapshotIDs: map[string]struct{}{"snap-1": {}},
		}, nil
	}

	snap := defaultSnapshot()
	snap.ListFunc = func(_ context.Context) ([]*types.Snapshot, error) {
		return []*types.Snapshot{
			{SnapshotConfig: types.SnapshotConfig{ID: "snap-1"}, CreatedAt: time.Now()},
			{SnapshotConfig: types.SnapshotConfig{ID: "snap-2"}, CreatedAt: time.Now()},
		}, nil
	}

	svc := newTestService(hyper, nil, nil, snap)

	snaps, err := svc.ListSnapshots(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snaps) != 1 {
		t.Errorf("expected 1 filtered snapshot, got %d", len(snaps))
	}

	if snaps[0].ID != "snap-1" {
		t.Errorf("expected snap-1, got %s", snaps[0].ID)
	}
}

func TestListSnapshots_VMNoSnapshots(t *testing.T) {
	hyper := defaultHypervisor()
	hyper.InspectFunc = func(_ context.Context, _ string) (*types.VM, error) {
		return &types.VM{ID: "vm-1", SnapshotIDs: nil}, nil
	}

	svc := newTestService(hyper, nil, nil, defaultSnapshot())

	snaps, err := svc.ListSnapshots(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snaps) != 0 {
		t.Errorf("expected 0 snapshots, got %d", len(snaps))
	}
}

func TestInspectSnapshot_Success(t *testing.T) {
	snap := defaultSnapshot()
	snap.InspectFunc = func(_ context.Context, ref string) (*types.Snapshot, error) {
		return &types.Snapshot{SnapshotConfig: types.SnapshotConfig{ID: ref, Name: "my-snap"}}, nil
	}

	svc := newTestService(defaultHypervisor(), nil, nil, snap)

	s, err := svc.InspectSnapshot(context.Background(), "snap-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if s.ID != "snap-1" {
		t.Errorf("expected snap-1, got %s", s.ID)
	}
}

func TestRemoveSnapshots_Success(t *testing.T) {
	snap := defaultSnapshot()
	svc := newTestService(defaultHypervisor(), nil, nil, snap)

	deleted, err := svc.RemoveSnapshots(context.Background(), []string{"snap-1", "snap-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(deleted) != 2 {
		t.Errorf("expected 2 deleted, got %d", len(deleted))
	}
}
