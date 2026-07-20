package hypervisor

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocoonstack/cocoon/lock/flock"
	meteringcapture "github.com/cocoonstack/cocoon/metering/capture"
	"github.com/cocoonstack/cocoon/storage"
	storejson "github.com/cocoonstack/cocoon/storage/json"
	"github.com/cocoonstack/cocoon/types"
)

type cloneStubConfig struct {
	stubBackendConfig
	root string
}

func (c cloneStubConfig) VMRunDir(id string) string { return filepath.Join(c.root, "run", id) }

func (c cloneStubConfig) VMLogDir(id string) string { return filepath.Join(c.root, "log", id) }

func (c cloneStubConfig) RunDir() string { return filepath.Join(c.root, "run") }

type countingStore struct {
	storage.Store[VMIndex]
	writes atomic.Int32
}

func (s *countingStore) Update(ctx context.Context, fn func(*VMIndex) error) error {
	s.writes.Add(1)
	return s.Store.Update(ctx, fn)
}

func (s *countingStore) UpdateNoDirSync(ctx context.Context, fn func(*VMIndex) error) error {
	s.writes.Add(1)
	return s.Store.UpdateNoDirSync(ctx, fn)
}

func newCloneTestBackend(t *testing.T) (*Backend, *countingStore) {
	t.Helper()
	dir := t.TempDir()
	locker := flock.New(filepath.Join(dir, "index.lock"))
	cs := &countingStore{Store: storejson.New[VMIndex](filepath.Join(dir, "index.json"), locker)}
	return &Backend{
		Typ:      "test-hv",
		Conf:     cloneStubConfig{root: dir},
		DB:       cs,
		Locker:   locker,
		Metering: meteringcapture.New(),
	}, cs
}

func cloneTestCfg(name string, preReserved bool) *types.VMConfig {
	return &types.VMConfig{
		Config:      types.Config{CPU: 1, Memory: 1 << 30, Storage: 10 << 30},
		Name:        name,
		PreReserved: preReserved,
	}
}

func TestCloneSetup_PreReservedSkipsIndexRewrite(t *testing.T) {
	b, cs := newCloneTestBackend(t)
	ctx := t.Context()
	const id = "vm-prereserved"
	blobs := map[string]struct{}{"blob1": {}}
	vmCfg := cloneTestCfg("c1", true)

	if err := b.PrereserveVM(ctx, id, vmCfg, blobs); err != nil {
		t.Fatalf("PrereserveVM: %v", err)
	}
	if got := cs.writes.Load(); got != 1 {
		t.Fatalf("writes after pre-reserve = %d, want 1", got)
	}

	runDir, logDir, _, cleanup, err := b.CloneSetup(ctx, id, vmCfg, &types.SnapshotConfig{ImageBlobIDs: blobs})
	if err != nil {
		t.Fatalf("CloneSetup: %v", err)
	}
	if got := cs.writes.Load(); got != 1 {
		t.Errorf("writes after CloneSetup = %d, want 1 (adoption must not rewrite the index)", got)
	}
	for _, d := range []string{runDir, logDir} {
		if _, statErr := os.Stat(d); statErr != nil {
			t.Errorf("dir %s not created: %v", d, statErr)
		}
	}

	cleanup()
	if err := b.DB.With(ctx, func(idx *VMIndex) error {
		if idx.VMs[id] != nil {
			t.Error("record survived cleanup")
		}
		if _, ok := idx.Names["c1"]; ok {
			t.Error("name mapping survived cleanup")
		}
		return nil
	}); err != nil {
		t.Fatalf("With: %v", err)
	}
	for _, d := range []string{runDir, logDir} {
		if _, statErr := os.Stat(d); !os.IsNotExist(statErr) {
			t.Errorf("dir %s survived cleanup", d)
		}
	}
}

func TestCloneSetup_DriftFallsBackToReserve(t *testing.T) {
	b, cs := newCloneTestBackend(t)
	ctx := t.Context()
	const id = "vm-drift"
	vmCfg := cloneTestCfg("c2", true)

	if err := b.PrereserveVM(ctx, id, vmCfg, map[string]struct{}{"old": {}}); err != nil {
		t.Fatalf("PrereserveVM: %v", err)
	}

	newBlobs := map[string]struct{}{"new": {}}
	if _, _, _, _, err := b.CloneSetup(ctx, id, vmCfg, &types.SnapshotConfig{ImageBlobIDs: newBlobs}); err != nil {
		t.Fatalf("CloneSetup: %v", err)
	}
	if got := cs.writes.Load(); got != 2 {
		t.Errorf("writes = %d, want 2 (blob drift must take the reserving path)", got)
	}
	if err := b.DB.With(ctx, func(idx *VMIndex) error {
		if _, ok := idx.VMs[id].ImageBlobIDs["new"]; !ok {
			t.Error("fallback did not refresh blob pins")
		}
		return nil
	}); err != nil {
		t.Fatalf("With: %v", err)
	}
}

func TestCloneSetup_NotPreReservedKeepsReservingPath(t *testing.T) {
	b, cs := newCloneTestBackend(t)
	ctx := t.Context()
	const id = "vm-plain"
	vmCfg := cloneTestCfg("c3", false)

	if _, _, _, _, err := b.CloneSetup(ctx, id, vmCfg, &types.SnapshotConfig{ImageBlobIDs: nil}); err != nil {
		t.Fatalf("CloneSetup: %v", err)
	}
	if got := cs.writes.Load(); got != 1 {
		t.Errorf("writes = %d, want 1 (non-pre-reserved caller must reserve)", got)
	}
	if err := b.DB.With(ctx, func(idx *VMIndex) error {
		if idx.VMs[id] == nil {
			t.Error("record missing after reserving CloneSetup")
		}
		return nil
	}); err != nil {
		t.Fatalf("With: %v", err)
	}
}
