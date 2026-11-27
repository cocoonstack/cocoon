package localfile

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/metering"
	meteringcapture "github.com/cocoonstack/cocoon/metering/capture"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestPickLRU(t *testing.T) {
	tests := []struct {
		name    string
		records map[string]snapshotMeta
		policy  EvictionPolicy
		want    []string
		reason  string
	}{
		{
			name: "NoCriteriaEvictsAll",
			records: map[string]snapshotMeta{
				"a": agedMeta(1, 100),
				"b": agedMeta(2, 100),
				"c": agedMeta(3, 100),
			},
			policy: EvictionPolicy{Enabled: true},
			want:   []string{"a", "b", "c"},
			reason: "lru-all",
		},
		{
			name: "KeepLast",
			records: map[string]snapshotMeta{
				"newest":   agedMeta(1, 10),
				"middle":   agedMeta(5, 10),
				"oldest":   agedMeta(10, 10),
				"oldester": agedMeta(20, 10),
			},
			policy: EvictionPolicy{Enabled: true, KeepLast: 2},
			want:   []string{"oldest", "oldester"},
			reason: "lru-keep",
		},
		{
			name:    "KeepLastExceedsAll",
			records: map[string]snapshotMeta{"a": agedMeta(1, 10), "b": agedMeta(2, 10)},
			policy:  EvictionPolicy{Enabled: true, KeepLast: 10},
		},
		{
			name: "MaxAge",
			records: map[string]snapshotMeta{
				"fresh": agedMeta(1, 10),
				"stale": agedMeta(48, 10),
			},
			policy: EvictionPolicy{Enabled: true, MaxAge: 24 * time.Hour},
			want:   []string{"stale"},
			reason: "lru-age",
		},
		{
			name: "MaxSize",
			records: map[string]snapshotMeta{
				"a": agedMeta(1, 30),
				"b": agedMeta(2, 30),
				"c": agedMeta(3, 30),
				"d": agedMeta(4, 30),
			},
			policy: EvictionPolicy{Enabled: true, MaxSize: 60},
			want:   []string{"c", "d"},
		},
		{
			name: "UnionOfCriteria",
			records: map[string]snapshotMeta{
				"fresh-small": agedMeta(1, 10),
				"fresh-big":   agedMeta(2, 100),
				"old-small":   agedMeta(48, 10),
			},
			policy: EvictionPolicy{Enabled: true, MaxAge: 24 * time.Hour, MaxSize: 50},
			want:   []string{"fresh-big", "old-small"},
		},
		{
			name: "ZeroTimeIsOldest",
			records: map[string]snapshotMeta{
				"recent": agedMeta(1, 10),
				"zero":   {lastAccessed: time.Time{}, sizeBytes: 10},
			},
			policy: EvictionPolicy{Enabled: true, KeepLast: 1},
			want:   []string{"zero"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickLRU(tt.records, tt.policy)
			if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, tt.want) {
				t.Errorf("evicted %v, want %v", keys, tt.want)
			}
			if tt.reason != "" && got[tt.want[0]] != tt.reason {
				t.Errorf("reason: got %q, want %q", got[tt.want[0]], tt.reason)
			}
		})
	}
}

func TestGCModule_LRUEndToEnd(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	createSnaps(t, lf, map[string][]byte{"cow.raw": []byte("x")}, "old1", "old2", "fresh")
	setLastAccessed(t, lf, time.Now().Add(-72*time.Hour), "old1", "old2")

	ids := collectGC(t, lf, EvictionPolicy{Enabled: true, MaxAge: 24 * time.Hour})
	if len(ids) != 2 {
		t.Errorf("want 2 evictions, got %v", ids)
	}

	remaining, err := lf.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Name != "fresh" {
		t.Errorf("after LRU: want only 'fresh', got %v", remaining)
	}
	for _, name := range []string{"old1", "old2"} {
		if _, err := lf.Inspect(ctx, name); err == nil {
			t.Errorf("%s should be deleted", name)
		}
	}
}

func TestGCModule_LRURevalidatesAccess(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	createSnaps(t, lf, map[string][]byte{"cow.raw": []byte("x")}, "old1", "old2")
	setLastAccessed(t, lf, time.Now().Add(-72*time.Hour), "old1", "old2")

	mod := gcModule(lf, EvictionPolicy{Enabled: true, MaxAge: 24 * time.Hour})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if len(ids) != 2 {
		t.Fatalf("want 2 candidates, got %v", ids)
	}

	setLastAccessed(t, lf, time.Now(), "old1")

	if err := mod.Collect(ctx, ids, snap); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	remaining, err := lf.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Name != "old1" {
		t.Fatalf("touched snapshot evicted on stale candidacy: %v", remaining)
	}
	if _, err := os.Stat(lf.conf.SnapshotDataDir(remaining[0].ID)); err != nil {
		t.Fatalf("survivor data dir: %v", err)
	}
}

func TestGCModule_DryRunNoEviction(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	createSnaps(t, lf, map[string][]byte{"x": []byte("x")}, "a", "b")

	policy := EvictionPolicy{Enabled: true, DryRun: true}
	mod := gcModule(lf, policy)
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if len(ids) != 0 {
		t.Errorf("dry-run should not return evictions, got %v", ids)
	}
}

func TestGCModule_BareSnapshotEvictsAll(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	createSnaps(t, lf, map[string][]byte{"x": []byte("x")}, "a", "b", "c")
	collectGC(t, lf, EvictionPolicy{Enabled: true})

	remaining, err := lf.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("bare --snapshot should evict all, got %v", remaining)
	}
}

func TestSizeAndLastAccessedAtPopulated(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	id := utils.GenerateID()
	before := time.Now()
	if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: "sized"},
		makeTar(t, map[string][]byte{"a": []byte("hello"), "b": []byte("world!!")})); err != nil {
		t.Fatal(err)
	}

	rec, err := lf.lookupRecord(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSize, err := utils.DirSize(lf.conf.SnapshotDataDir(id))
	if err != nil {
		t.Fatal(err)
	}
	if rec.SizeBytes != wantSize {
		t.Errorf("SizeBytes=%d, want %d", rec.SizeBytes, wantSize)
	}
	if rec.LastAccessedAt.Before(before) {
		t.Errorf("LastAccessedAt %v not after %v", rec.LastAccessedAt, before)
	}
}

func TestRestoreUpdatesLastAccessedAt(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	id := utils.GenerateID()
	if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: "touched"},
		makeTar(t, map[string][]byte{"x": []byte("x")})); err != nil {
		t.Fatal(err)
	}

	original := time.Now().Add(-48 * time.Hour)
	setLastAccessed(t, lf, original, "touched")

	if _, rc, err := lf.Restore(ctx, "touched"); err != nil {
		t.Fatal(err)
	} else {
		rc.Close()
	}

	rec, err := lf.lookupRecord(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.LastAccessedAt.After(original) {
		t.Errorf("LastAccessedAt not updated: still %v", rec.LastAccessedAt)
	}
}

func TestGCModule_RemovalFailureKeepsDBRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod restrictions")
	}
	lf := newTestLF(t)
	ctx := t.Context()

	ids := createSnaps(t, lf, map[string][]byte{"x": []byte("x")}, "a", "b")

	mod := gcModule(lf, EvictionPolicy{Enabled: true})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}

	for _, id := range ids {
		dir := lf.conf.SnapshotDataDir(id)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Skipf("chmod failed: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	}

	err = mod.Collect(ctx, ids, snap)
	if err == nil || !strings.Contains(err.Error(), "tombstone kept") {
		t.Fatalf("Collect error = %v, want a data dir removal failure keeping the tombstone", err)
	}
	for i, name := range []string{"a", "b"} {
		if _, err := lf.lookupRecord(ctx, ids[i], false); err != nil {
			t.Errorf("%s: DB record should survive removal failure, got: %v", name, err)
		}
		if !snapTombstonePresent(t, lf, ids[i]) {
			t.Errorf("%s: tombstone should be kept after removal failure", name)
		}
	}
}

func TestGCModule_OrphanDirCleaned(t *testing.T) {
	lf := newTestLF(t)

	orphanDir := filepath.Join(lf.conf.DataDir(), "ORPHAN_ID_NO_DB")
	if err := os.MkdirAll(orphanDir, 0o750); err != nil {
		t.Fatal(err)
	}

	ids := collectGC(t, lf, EvictionPolicy{})
	if !slices.Contains(ids, "ORPHAN_ID_NO_DB") {
		t.Errorf("orphan dir should be picked, got %v", ids)
	}
	if _, err := os.Stat(orphanDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("orphan dir should be removed, stat err: %v", err)
	}
}

func TestGCModule_EvictRealRecordEmitsSnapStorageStop(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	for _, name := range []string{"snap-a", "snap-b"} {
		id := utils.GenerateID()
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: name, Hypervisor: "cloud-hypervisor"},
			makeTar(t, map[string][]byte{"cow.raw": []byte("x")})); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	rec := meteringcapture.New()
	if ids := collectGC(t, withRecorder(lf, rec), EvictionPolicy{Enabled: true}); len(ids) != 2 {
		t.Fatalf("want 2 ids to evict, got %v", ids)
	}

	entries := rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (one stop per evicted record)", len(entries))
	}
	for _, e := range entries {
		if e.Kind != metering.KindSnapStorageStop {
			t.Errorf("kind = %s, want snap.storage.stop", e.Kind)
		}
		if e.Reason != metering.ReasonSnapRemove {
			t.Errorf("reason = %q, want snap-rm", e.Reason)
		}
		if e.Hypervisor != "cloud-hypervisor" {
			t.Errorf("hypervisor = %q, want cloud-hypervisor", e.Hypervisor)
		}
	}
}

func TestGCModule_OrphanAndStalePendingDoNotEmit(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	orphanDir := filepath.Join(lf.conf.DataDir(), "ORPHAN_ID")
	if err := os.MkdirAll(orphanDir, 0o750); err != nil {
		t.Fatal(err)
	}
	stalePendingID := utils.GenerateID()
	if err := lf.update(ctx, func(tx *snapTx) error {
		return tx.Put(stalePendingID, &snapshotRecord{
			ID:        stalePendingID,
			CreatedAt: time.Now().Add(-48 * time.Hour),
			Pending:   true,
		})
	}); err != nil {
		t.Fatal(err)
	}

	rec := meteringcapture.New()
	ids := collectGC(t, withRecorder(lf, rec), EvictionPolicy{})
	if !slices.Contains(ids, "ORPHAN_ID") || !slices.Contains(ids, stalePendingID) {
		t.Fatalf("want both orphan and stale-pending picked, got %v", ids)
	}
	if got := rec.Entries(); len(got) != 0 {
		t.Errorf("got %d entries; orphan/stale-pending must not emit stop", len(got))
	}
}

func TestGCCollectsFreshPendingWithFreeLease(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	id := utils.GenerateID()
	if _, err := lf.beginCreate(ctx, &types.SnapshotConfig{ID: id, Name: "fresh-pending"}); err != nil {
		t.Fatalf("beginCreate: %v", err)
	}
	if err := os.MkdirAll(lf.conf.SnapshotDataDir(id), 0o750); err != nil {
		t.Fatal(err)
	}

	if ids := collectGC(t, lf, EvictionPolicy{}); !slices.Contains(ids, id) {
		t.Fatalf("candidates = %v, want the fresh pending record picked", ids)
	}
	if _, held, err := lf.NameOwner(ctx, "fresh-pending"); err != nil || held {
		t.Fatalf("NameOwner = (%v, %v), want the dead save's name freed", err, held)
	}
	if _, err := os.Stat(lf.conf.SnapshotDataDir(id)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("data dir survives: %v", err)
	}
}

func TestGCSkipsPendingHeldByLiveBuild(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	id := utils.GenerateID()
	release, err := lf.acquireBuildLease(ctx, id)
	if err != nil {
		t.Fatalf("acquireBuildLease: %v", err)
	}
	defer release()
	if _, err := lf.beginCreate(ctx, &types.SnapshotConfig{ID: id, Name: "live-build"}); err != nil {
		t.Fatalf("beginCreate: %v", err)
	}

	collectGC(t, lf, EvictionPolicy{})
	if owner, held, err := lf.NameOwner(ctx, "live-build"); err != nil || !held || owner != id {
		t.Fatalf("NameOwner = (%q, %v, %v), want the live build's record untouched", owner, held, err)
	}
}

func TestDeleteReclaimsTheLeaseFile(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	id, err := lf.Create(ctx, &types.SnapshotConfig{
		ID: utils.GenerateID(), Name: "leased", Hypervisor: "cloud-hypervisor",
	}, makeTar(t, map[string][]byte{"cow.raw": []byte("x")}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := lf.deleteOne(ctx, id); err != nil {
		t.Fatalf("deleteOne: %v", err)
	}
	if _, err := os.Stat(lf.conf.LeasePath(id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lease file survived delete: stat err = %v", err)
	}
}

func TestGCSweepsALeaseWithNoRecordOrDataDir(t *testing.T) {
	lf := newTestLF(t)
	stale := lf.conf.LeasePath("GONE_ID")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	collectGC(t, lf, EvictionPolicy{})
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale lease survived GC: stat err = %v", err)
	}
}

func TestGCCollectsACrashedBuildAndItsLeaseInOnePass(t *testing.T) {
	lf := newTestLF(t)
	id := utils.GenerateID()
	dataDir := lf.conf.SnapshotDataDir(id)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "cow.raw"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lf.conf.LeasePath(id), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	collectGC(t, lf, EvictionPolicy{})
	if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphan data dir survived GC: stat err = %v", err)
	}
	if _, err := os.Stat(lf.conf.LeasePath(id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("crashed build's lease survived GC: stat err = %v", err)
	}
}

func TestGCKeepsALeaseHeldByALiveBuild(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	id := utils.GenerateID()
	release, err := lf.acquireBuildLease(ctx, id)
	if err != nil {
		t.Fatalf("acquireBuildLease: %v", err)
	}
	defer release()

	collectGC(t, lf, EvictionPolicy{})
	if _, err := os.Stat(lf.conf.LeasePath(id)); err != nil {
		t.Fatalf("GC swept a lease held by a live build: %v", err)
	}
}

func TestGCKeepsSnapshotsAfterTheRootMoved(t *testing.T) {
	ctx := t.Context()
	base := t.TempDir()
	oldLF := newTestLFAt(t, filepath.Join(base, "old"), metering.NopRecorder{})
	id := makeExportableSnapshot(t, oldLF, "moved", map[string][]byte{"disk.raw": []byte("data")})
	staleDir := oldLF.conf.SnapshotDataDir(id)

	conf := &config.Config{RootDir: filepath.Join(base, "new")}
	if err := os.Rename(oldLF.conf.RootDir, conf.RootDir); err != nil {
		t.Fatal(err)
	}
	indexPath := NewConfig(conf).IndexFile()
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]map[string]any
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	record, ok := index["snapshots"][id].(map[string]any)
	if !ok {
		t.Fatalf("no row for %s in %s", id, raw)
	}
	record["data_dir"] = staleDir
	if raw, err = json.Marshal(index); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	lf, err := New(conf, metering.NopRecorder{}, newTestMetaStore(t, conf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ids := collectGC(t, lf, EvictionPolicy{}); len(ids) != 0 {
		t.Errorf("GC candidates = %v, want none", ids)
	}

	dir, _, release, err := lf.DataDir(ctx, id)
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	defer release()
	if want := lf.conf.SnapshotDataDir(id); dir != want {
		t.Errorf("DataDir = %s, want %s", dir, want)
	}
	if _, err := os.Stat(filepath.Join(lf.conf.SnapshotDataDir(id), "disk.raw")); err != nil {
		t.Errorf("snapshot data lost: %v", err)
	}
}

func agedMeta(ageHours int, size int64) snapshotMeta {
	accessedAt := time.Now().Add(-time.Duration(ageHours) * time.Hour)
	return snapshotMeta{lastAccessed: accessedAt, sizeBytes: size}
}

func collectGC(t *testing.T, lf *LocalFile, policy EvictionPolicy) []string {
	t.Helper()
	ctx := t.Context()
	mod := gcModule(lf, policy)
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if err := mod.Collect(ctx, ids, snap); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return ids
}

func createSnaps(t *testing.T, lf *LocalFile, files map[string][]byte, names ...string) []string {
	t.Helper()
	ids := make([]string, 0, len(names))
	for _, name := range names {
		id, err := lf.Create(t.Context(), &types.SnapshotConfig{ID: utils.GenerateID(), Name: name}, makeTar(t, files))
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func setLastAccessed(t *testing.T, lf *LocalFile, at time.Time, refs ...string) {
	t.Helper()
	if err := lf.update(t.Context(), func(tx *snapTx) error {
		for _, ref := range refs {
			id, err := tx.Resolve(ref, snapshot.ErrNotFound)
			if err != nil {
				return err
			}
			rec, err := tx.Get(id)
			if err != nil {
				return err
			}
			if rec == nil {
				return fmt.Errorf("snapshot %q missing", ref)
			}
			rec.LastAccessedAt = at
			if err := tx.Put(id, rec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func withRecorder(lf *LocalFile, rec metering.Recorder) *LocalFile {
	lf.metering = rec
	return lf
}
