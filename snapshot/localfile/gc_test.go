package localfile

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/metering"
	meteringcapture "github.com/cocoonstack/cocoon/metering/capture"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
)

func TestPickLRU_NoCriteriaEvictsAll(t *testing.T) {
	records := map[string]snapshotMeta{
		"a": agedMeta(1, 100),
		"b": agedMeta(2, 100),
		"c": agedMeta(3, 100),
	}
	got := pickLRU(records, EvictionPolicy{Enabled: true})
	if len(got) != 3 {
		t.Fatalf("want 3 evictions, got %v", got)
	}
}

func TestPickLRU_KeepLast(t *testing.T) {
	records := map[string]snapshotMeta{
		"newest":   agedMeta(1, 10),
		"middle":   agedMeta(5, 10),
		"oldest":   agedMeta(10, 10),
		"oldester": agedMeta(20, 10),
	}
	got := pickLRU(records, EvictionPolicy{Enabled: true, KeepLast: 2})
	if !slices.Equal(sortedKeys(got), []string{"oldest", "oldester"}) {
		t.Errorf("KeepLast=2: got %v", got)
	}
	if got["oldest"] != "lru-keep" {
		t.Errorf("reason: got %q, want lru-keep", got["oldest"])
	}
}

func TestPickLRU_KeepLastExceedsAll(t *testing.T) {
	records := map[string]snapshotMeta{"a": agedMeta(1, 10), "b": agedMeta(2, 10)}
	got := pickLRU(records, EvictionPolicy{Enabled: true, KeepLast: 10})
	if len(got) != 0 {
		t.Errorf("KeepLast>len: got %v, want empty", got)
	}
}

func TestPickLRU_MaxAge(t *testing.T) {
	records := map[string]snapshotMeta{
		"fresh": agedMeta(1, 10),
		"stale": agedMeta(48, 10),
	}
	got := pickLRU(records, EvictionPolicy{Enabled: true, MaxAge: 24 * time.Hour})
	if !slices.Equal(sortedKeys(got), []string{"stale"}) {
		t.Errorf("MaxAge=24h: got %v", got)
	}
	if got["stale"] != "lru-age" {
		t.Errorf("reason: got %q, want lru-age", got["stale"])
	}
}

func TestPickLRU_MaxSize(t *testing.T) {
	records := map[string]snapshotMeta{
		"a": agedMeta(1, 30),
		"b": agedMeta(2, 30),
		"c": agedMeta(3, 30),
		"d": agedMeta(4, 30),
	}
	got := pickLRU(records, EvictionPolicy{Enabled: true, MaxSize: 60})
	if !slices.Equal(sortedKeys(got), []string{"c", "d"}) {
		t.Errorf("MaxSize=60: got %v", got)
	}
}

func TestPickLRU_UnionOfCriteria(t *testing.T) {
	records := map[string]snapshotMeta{
		"fresh-small": agedMeta(1, 10),
		"fresh-big":   agedMeta(2, 100),
		"old-small":   agedMeta(48, 10),
	}
	got := pickLRU(records, EvictionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSize: 50,
	})
	if !slices.Equal(sortedKeys(got), []string{"fresh-big", "old-small"}) {
		t.Errorf("union: got %v", got)
	}
}

func TestPickLRU_ZeroTimeIsOldest(t *testing.T) {
	records := map[string]snapshotMeta{
		"recent": agedMeta(1, 10),
		"zero":   {lastAccessed: time.Time{}, sizeBytes: 10},
	}
	got := pickLRU(records, EvictionPolicy{Enabled: true, KeepLast: 1})
	if !slices.Equal(sortedKeys(got), []string{"zero"}) {
		t.Errorf("zero time should be evicted first: got %v", got)
	}
}

func TestPickLRU_NoCriteriaAllReasonAll(t *testing.T) {
	records := map[string]snapshotMeta{"a": agedMeta(1, 10)}
	got := pickLRU(records, EvictionPolicy{Enabled: true})
	if got["a"] != "lru-all" {
		t.Errorf("reason: got %q, want lru-all", got["a"])
	}
}

func TestGCModule_LRUEndToEnd(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	for _, name := range []string{"old1", "old2", "fresh"} {
		id := testID(t)
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: name},
			makeTar(t, map[string][]byte{"cow.raw": []byte("x")})); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	pastAccess := time.Now().Add(-72 * time.Hour)
	if err := lf.dbUpdate(ctx, func(idx *snapshotIndex) error {
		for _, name := range []string{"old1", "old2"} {
			r := idx.Snapshots[idx.Names[name]]
			if r == nil {
				return fmt.Errorf("setup: %s record missing", name)
			}
			r.LastAccessedAt = pastAccess
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	policy := EvictionPolicy{Enabled: true, MaxAge: 24 * time.Hour}
	mod := gcModule(lf, policy)
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if len(ids) != 2 {
		t.Errorf("want 2 evictions, got %v", ids)
	}
	if err := mod.Collect(ctx, ids, snap); err != nil {
		t.Fatalf("Collect: %v", err)
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

// TestGCModule_LRURevalidatesAccess pins design §5 step 2: a snapshot touched between ReadDB and Collect must survive the stale candidate list.
func TestGCModule_LRURevalidatesAccess(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	for _, name := range []string{"old1", "old2"} {
		id := testID(t)
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: name},
			makeTar(t, map[string][]byte{"cow.raw": []byte("x")})); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	pastAccess := time.Now().Add(-72 * time.Hour)
	if err := lf.dbUpdate(ctx, func(idx *snapshotIndex) error {
		for _, name := range []string{"old1", "old2"} {
			idx.Snapshots[idx.Names[name]].LastAccessedAt = pastAccess
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	mod := gcModule(lf, EvictionPolicy{Enabled: true, MaxAge: 24 * time.Hour})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if len(ids) != 2 {
		t.Fatalf("want 2 candidates, got %v", ids)
	}
	// A clone/export lands in the window and touches old1.
	if err := lf.dbUpdate(ctx, func(idx *snapshotIndex) error {
		idx.Snapshots[idx.Names["old1"]].LastAccessedAt = time.Now()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

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

	for _, name := range []string{"a", "b"} {
		id := testID(t)
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: name},
			makeTar(t, map[string][]byte{"x": []byte("x")})); err != nil {
			t.Fatal(err)
		}
	}

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

	for _, name := range []string{"a", "b", "c"} {
		id := testID(t)
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: name},
			makeTar(t, map[string][]byte{"x": []byte("x")})); err != nil {
			t.Fatal(err)
		}
	}

	mod := gcModule(lf, EvictionPolicy{Enabled: true})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if err := mod.Collect(ctx, ids, snap); err != nil {
		t.Fatal(err)
	}

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

	id := testID(t)
	before := time.Now()
	if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: "sized"},
		makeTar(t, map[string][]byte{"a": []byte("hello"), "b": []byte("world!!")})); err != nil {
		t.Fatal(err)
	}

	rec, err := lf.lookupRecord(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSize := int64(len("hello") + len("world!!"))
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

	id := testID(t)
	if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: "touched"},
		makeTar(t, map[string][]byte{"x": []byte("x")})); err != nil {
		t.Fatal(err)
	}

	original := time.Now().Add(-48 * time.Hour)
	if err := lf.dbUpdate(ctx, func(idx *snapshotIndex) error {
		r := idx.Snapshots[id]
		if r == nil {
			return fmt.Errorf("setup: %s missing", id)
		}
		r.LastAccessedAt = original
		return nil
	}); err != nil {
		t.Fatal(err)
	}

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

	ids := []string{testID(t), testID(t)}
	for i, name := range []string{"a", "b"} {
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: ids[i], Name: name},
			makeTar(t, map[string][]byte{"x": []byte("x")})); err != nil {
			t.Fatal(err)
		}
	}

	mod := gcModule(lf, EvictionPolicy{Enabled: true})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}

	parent := lf.conf.DataDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Skipf("chmod failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o750) })

	if err := mod.Collect(ctx, ids, snap); err == nil {
		t.Fatal("expected Collect to error on chmod-protected parent")
	}
	for i, name := range []string{"a", "b"} {
		if _, err := lf.lookupRecord(ctx, ids[i], false); err != nil {
			t.Errorf("%s: DB record should survive removal failure, got: %v", name, err)
		}
	}
}

func TestGCModule_OrphanDirCleaned(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	orphanDir := filepath.Join(lf.conf.DataDir(), "ORPHAN_ID_NO_DB")
	if err := os.MkdirAll(orphanDir, 0o750); err != nil {
		t.Fatal(err)
	}

	mod := gcModule(lf, EvictionPolicy{})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if !slices.Contains(ids, "ORPHAN_ID_NO_DB") {
		t.Errorf("orphan dir should be picked, got %v", ids)
	}
	if err := mod.Collect(ctx, ids, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan dir should be removed, stat err: %v", err)
	}
}

func TestGCModule_EvictRealRecordEmitsSnapStorageStop(t *testing.T) {
	// LRU-evicting a real record must close its ledger interval (snap.storage.stop).
	lf := newTestLF(t)
	ctx := t.Context()

	for _, name := range []string{"snap-a", "snap-b"} {
		id := testID(t)
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: name, Hypervisor: "cloud-hypervisor"},
			makeTar(t, map[string][]byte{"cow.raw": []byte("x")})); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	rec := meteringcapture.New()
	mod := gcModule(withRecorder(lf, rec), EvictionPolicy{Enabled: true})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if len(ids) != 2 {
		t.Fatalf("want 2 ids to evict, got %v", ids)
	}
	if err := mod.Collect(ctx, ids, snap); err != nil {
		t.Fatal(err)
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
	// Orphan and stale-pending records never opened a ledger interval, so GC must not emit a phantom stop.
	lf := newTestLF(t)
	ctx := t.Context()

	orphanDir := filepath.Join(lf.conf.DataDir(), "ORPHAN_ID")
	if err := os.MkdirAll(orphanDir, 0o750); err != nil {
		t.Fatal(err)
	}
	stalePendingID := testID(t)
	if err := lf.dbUpdate(ctx, func(idx *snapshotIndex) error {
		idx.Snapshots[stalePendingID] = &snapshot.SnapshotRecord{
			Snapshot: types.Snapshot{
				SnapshotConfig: types.SnapshotConfig{ID: stalePendingID},
				CreatedAt:      time.Now().Add(-48 * time.Hour),
			},
			Pending: true,
			DataDir: filepath.Join(lf.conf.DataDir(), stalePendingID),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rec := meteringcapture.New()
	mod := gcModule(withRecorder(lf, rec), EvictionPolicy{})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if !slices.Contains(ids, "ORPHAN_ID") || !slices.Contains(ids, stalePendingID) {
		t.Fatalf("want both orphan and stale-pending picked, got %v", ids)
	}
	if err := mod.Collect(ctx, ids, snap); err != nil {
		t.Fatal(err)
	}
	if got := rec.Entries(); len(got) != 0 {
		t.Errorf("got %d entries; orphan/stale-pending must not emit stop", len(got))
	}
}

// TestGCCollectsFreshPendingWithFreeLease pins the ownerless proof: a dead save's pending record is reclaimed on the next pass however young — the free build lease is the proof, not an age gate.
func TestGCCollectsFreshPendingWithFreeLease(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	id := testID(t)
	if _, err := lf.beginCreate(ctx, &types.SnapshotConfig{ID: id, Name: "fresh-pending"}); err != nil {
		t.Fatalf("beginCreate: %v", err)
	}
	if err := os.MkdirAll(lf.conf.SnapshotDataDir(id), 0o750); err != nil {
		t.Fatal(err)
	}

	mod := gcModule(lf, EvictionPolicy{})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := mod.Resolve(ctx, snap, map[string]any{})
	if !slices.Contains(ids, id) {
		t.Fatalf("candidates = %v, want the fresh pending record picked", ids)
	}
	if err := mod.Collect(ctx, ids, snap); err != nil {
		t.Fatal(err)
	}
	if _, held, err := lf.NameOwner(ctx, "fresh-pending"); err != nil || held {
		t.Fatalf("NameOwner = (%v, %v), want the dead save's name freed", err, held)
	}
	if _, err := os.Stat(lf.conf.SnapshotDataDir(id)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("data dir survives: %v", err)
	}
}

// TestGCSkipsPendingHeldByLiveBuild pins the other half of the proof: a save still holding its build lease must not lose its pending record to GC.
func TestGCSkipsPendingHeldByLiveBuild(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	id := testID(t)
	release, err := lf.acquireBuildLease(id)
	if err != nil {
		t.Fatalf("acquireBuildLease: %v", err)
	}
	defer release()
	if _, err := lf.beginCreate(ctx, &types.SnapshotConfig{ID: id, Name: "live-build"}); err != nil {
		t.Fatalf("beginCreate: %v", err)
	}

	mod := gcModule(lf, EvictionPolicy{})
	snap, err := mod.ReadDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := mod.Collect(ctx, mod.Resolve(ctx, snap, map[string]any{}), snap); err != nil {
		t.Fatal(err)
	}
	if owner, held, err := lf.NameOwner(ctx, "live-build"); err != nil || !held || owner != id {
		t.Fatalf("NameOwner = (%q, %v, %v), want the live build's record untouched", owner, held, err)
	}
}

func agedMeta(ageHours int, size int64) snapshotMeta {
	accessedAt := time.Now().Add(-time.Duration(ageHours) * time.Hour)
	return snapshotMeta{lastAccessed: accessedAt, sizeBytes: size}
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

func withRecorder(lf *LocalFile, rec metering.Recorder) *LocalFile {
	lf.metering = rec
	return lf
}
