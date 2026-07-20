package localfile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/metering"
	meteringcapture "github.com/cocoonstack/cocoon/metering/capture"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestNew(t *testing.T) {
	dir := t.TempDir()
	lf, err := New(&config.Config{RootDir: dir}, metering.NopRecorder{}, newTestMetaStore(t, &config.Config{RootDir: dir}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if lf == nil {
		t.Fatal("expected non-nil LocalFile")
	}
}

func TestNew_NilConfig(t *testing.T) {
	_, err := New(nil, metering.NopRecorder{}, nil)
	if err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestCreateAndDeleteEmitMetering(t *testing.T) {
	rec := meteringcapture.New()
	lf := newTestLFWithRecorder(t, rec)
	ctx := t.Context()

	id, err := lf.Create(ctx, &types.SnapshotConfig{
		ID:         testID(t),
		Name:       "metered-snap",
		Hypervisor: "cloud-hypervisor",
	}, makeTar(t, map[string][]byte{"cow.raw": []byte("disk")}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries := rec.Entries()
	if len(entries) != 1 {
		t.Fatalf("after Create: got %d entries, want 1", len(entries))
	}
	if entries[0].Kind != metering.KindSnapStorageStart || entries[0].SnapshotID != id ||
		entries[0].Hypervisor != "cloud-hypervisor" || entries[0].Shape.StorageBytes <= 0 {
		t.Errorf("snap.storage.start entry wrong: %+v", entries[0])
	}

	if _, err := lf.Delete(ctx, []string{id}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	entries = rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("after Delete: got %d entries, want 2", len(entries))
	}
	if entries[1].Kind != metering.KindSnapStorageStop || entries[1].SnapshotID != id ||
		entries[1].Reason != metering.ReasonSnapRemove || entries[1].Hypervisor != "cloud-hypervisor" {
		t.Errorf("snap.storage.stop entry wrong: %+v", entries[1])
	}
}

func TestDeleteOneIdempotentDoesNotEmitTwice(t *testing.T) {
	// Racing rm: the loser's closure sees a nil rec and must report success
	// without emitting a phantom stop. deleteOne twice on one id simulates it.
	rec := meteringcapture.New()
	lf := newTestLFWithRecorder(t, rec)
	ctx := t.Context()

	id, err := lf.Create(ctx, &types.SnapshotConfig{
		ID: testID(t), Name: "raced", Hypervisor: "cloud-hypervisor",
	}, makeTar(t, map[string][]byte{"cow.raw": []byte("x")}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := lf.deleteOne(ctx, id); err != nil {
		t.Fatalf("first deleteOne: %v", err)
	}
	if err := lf.deleteOne(ctx, id); err != nil {
		t.Fatalf("second deleteOne (idempotent): %v", err)
	}

	// Exactly 2 entries: Create's start + the first deleteOne's stop.
	entries := rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (start + 1× stop); kinds = %v", len(entries), kinds(entries))
	}
	if entries[0].Kind != metering.KindSnapStorageStart {
		t.Errorf("entries[0] kind = %s, want snap.storage.start", entries[0].Kind)
	}
	if entries[1].Kind != metering.KindSnapStorageStop {
		t.Errorf("entries[1] kind = %s, want snap.storage.stop", entries[1].Kind)
	}
	if entries[1].Hypervisor != "cloud-hypervisor" {
		t.Errorf("stop entry has Hypervisor=%q; phantom emits leak as empty", entries[1].Hypervisor)
	}
}

func TestCreateFromDirDirectMatchesCreateLayout(t *testing.T) {
	rec := meteringcapture.New()
	lf := newTestLFWithRecorder(t, rec)
	ctx := t.Context()

	files := map[string][]byte{
		"memory-range-0": []byte("guest-ram"),
		"config.json":    []byte(`{"disks":[]}`),
		"cocoon.json":    []byte(`{"storage_configs":[]}`),
	}

	// A capture dir under the store root shares its filesystem, so the direct
	// (rename) path is taken rather than the cross-fs streaming fallback.
	srcDir := writeCaptureDir(t, filepath.Join(lf.conf.RootDir, "capture-src"), files)
	id, ok, err := lf.CreateFromDir(ctx, &types.SnapshotConfig{
		ID: testID(t), Name: "direct-snap", Hypervisor: "cloud-hypervisor",
	}, srcDir)
	if err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	if !ok {
		t.Fatal("CreateFromDir took the fallback path; expected direct (same filesystem)")
	}
	if _, statErr := os.Stat(srcDir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("srcDir survived the move: stat err = %v", statErr)
	}
	if _, err := lf.Inspect(ctx, id); err != nil {
		t.Fatalf("Inspect after CreateFromDir: %v", err)
	}
	if entries := rec.Entries(); len(entries) != 1 || entries[0].Kind != metering.KindSnapStorageStart {
		t.Fatalf("metering = %v, want one snap.storage.start", kinds(rec.Entries()))
	}

	// The on-disk layout is byte-for-byte what the tar path (Create) produces.
	id2, err := lf.Create(ctx, &types.SnapshotConfig{
		ID: testID(t), Name: "streamed-snap", Hypervisor: "cloud-hypervisor",
	}, makeTar(t, files))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	direct := readDirFiles(t, lf.conf.SnapshotDataDir(id))
	streamed := readDirFiles(t, lf.conf.SnapshotDataDir(id2))
	if !maps.Equal(direct, streamed) {
		t.Errorf("direct layout %v differs from tar layout %v", direct, streamed)
	}
}

func TestCreateFromDirEXDEVFallsBack(t *testing.T) {
	rec := meteringcapture.New()
	lf := newTestLFWithRecorder(t, rec)
	ctx := t.Context()

	// st_dev matches but rename EXDEVs (as on a bind mount): must roll back the pending record, leave srcDir intact, and report ok=false.
	orig := osRename
	osRename = func(string, string) error { return syscall.EXDEV }
	t.Cleanup(func() { osRename = orig })

	id := testID(t)
	srcDir := writeCaptureDir(t, filepath.Join(lf.conf.RootDir, "capture-exdev"), map[string][]byte{
		"memory-range-0": []byte("ram"),
	})
	gotID, ok, err := lf.CreateFromDir(ctx, &types.SnapshotConfig{
		ID: id, Name: "exdev-snap", Hypervisor: "cloud-hypervisor",
	}, srcDir)
	if err != nil {
		t.Fatalf("CreateFromDir on EXDEV: unexpected err = %v", err)
	}
	if ok || gotID != "" {
		t.Fatalf("CreateFromDir = (%q, %v), want fallback (\"\", false)", gotID, ok)
	}
	if _, statErr := os.Stat(srcDir); statErr != nil {
		t.Errorf("srcDir removed on EXDEV fallback: %v", statErr)
	}
	// Check the index directly: Inspect hides pending records, so it cannot
	// distinguish "rolled back" from "stale pending left behind" — and a stale
	// name reservation would fail the tar-fallback Create with "already in use".
	if err := lf.dbRead(ctx, func(idx *snapshot.SnapshotIndex) error {
		if _, stale := idx.Snapshots[id]; stale {
			return fmt.Errorf("pending record %s still in index", id)
		}
		if _, stale := idx.Names["exdev-snap"]; stale {
			return fmt.Errorf("name %q still reserved", "exdev-snap")
		}
		return nil
	}); err != nil {
		t.Errorf("EXDEV rollback incomplete: %v", err)
	}
	if n := len(rec.Entries()); n != 0 {
		t.Errorf("metering emitted %d entries on fallback, want 0", n)
	}
}

func TestImportEmitsSnapStorageStart(t *testing.T) {
	rec := meteringcapture.New()
	lf := newTestLFWithRecorder(t, rec)
	ctx := t.Context()

	envelope, err := snapshot.MarshalEnvelope(types.SnapshotConfig{
		ID:         "src-snap",
		Name:       "src-name",
		Hypervisor: "cloud-hypervisor",
	})
	if err != nil {
		t.Fatalf("MarshalEnvelope: %v", err)
	}
	stream := makeTar(t, map[string][]byte{
		snapshot.SnapshotJSONName: envelope,
		"cow.raw":                 []byte("disk-data"),
	})

	id, err := lf.Import(ctx, stream, "imported", "from test")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	entries := rec.Entries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (snap.storage.start)", len(entries))
	}
	e := entries[0]
	if e.Kind != metering.KindSnapStorageStart || e.SnapshotID != id ||
		e.Hypervisor != "cloud-hypervisor" || e.Shape.StorageBytes <= 0 {
		t.Errorf("entry wrong: %+v", e)
	}
}

func TestRollbackCreateSurvivesCanceledContext(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	id := testID(t)
	if _, err := lf.beginCreate(ctx, &types.SnapshotConfig{ID: id, Name: "pending-snap"}); err != nil {
		t.Fatalf("beginCreate: %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	lf.rollbackCreate(cctx, id, "pending-snap")

	if _, err := lf.Inspect(ctx, id); err == nil {
		t.Fatal("pending record must be rolled back even under a canceled context")
	}
	if _, err := lf.Inspect(ctx, "pending-snap"); err == nil {
		t.Fatal("name mapping must be released even under a canceled context")
	}
}

func TestCreate(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{
		"cow.raw":    []byte("disk data"),
		"state.json": []byte(`{"state":"ok"}`),
	})

	cfg := &types.SnapshotConfig{
		ID:          testID(t),
		Name:        "snap1",
		Description: "test snapshot",
		ImageBlobIDs: map[string]struct{}{
			"abc123": {},
		},
	}

	id, err := lf.Create(ctx, cfg, stream)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}

	// Verify data files were extracted.
	dataDir := lf.conf.SnapshotDataDir(id)
	for _, name := range []string{"cow.raw", "state.json"} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err != nil {
			t.Errorf("expected %s in data dir: %v", name, err)
		}
	}
}

func TestCreate_NoName(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t)}, stream)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}
}

func TestCreate_DuplicateName(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	cfg := &types.SnapshotConfig{ID: testID(t), Name: "dup"}

	stream1 := makeTar(t, map[string][]byte{"a.txt": []byte("a")})
	if _, err := lf.Create(ctx, cfg, stream1); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	cfg2 := &types.SnapshotConfig{ID: testID(t), Name: "dup"}
	stream2 := makeTar(t, map[string][]byte{"b.txt": []byte("b")})
	_, err := lf.Create(ctx, cfg2, stream2)
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCreate_InvalidStream(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	_, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "bad"}, strings.NewReader("not gzip"))
	if err == nil {
		t.Fatal("expected error for invalid stream")
	}
}

func TestList_Empty(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	result, err := lf.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 snapshots, got %d", len(result))
	}
}

func TestList(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	for _, name := range []string{"s1", "s2", "s3"} {
		stream := makeTar(t, map[string][]byte{"f.txt": []byte(name)})
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: name}, stream); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	result, err := lf.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("expected 3 snapshots, got %d", len(result))
	}

	names := make(map[string]bool)
	for _, s := range result {
		names[s.Name] = true
	}
	for _, name := range []string{"s1", "s2", "s3"} {
		if !names[name] {
			t.Errorf("missing snapshot %q", name)
		}
	}
}

func TestInspect_ByID(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "byid", Description: "desc"}, stream)
	if err != nil {
		t.Fatal(err)
	}

	s, err := lf.Inspect(ctx, id)
	if err != nil {
		t.Fatalf("Inspect by ID: %v", err)
	}
	if s.ID != id {
		t.Errorf("ID: got %q, want %q", s.ID, id)
	}
	if s.Name != "byid" {
		t.Errorf("Name: got %q, want %q", s.Name, "byid")
	}
	if s.Description != "desc" {
		t.Errorf("Description: got %q, want %q", s.Description, "desc")
	}
}

func TestInspect_ByName(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "byname"}, stream)
	if err != nil {
		t.Fatal(err)
	}

	s, err := lf.Inspect(ctx, "byname")
	if err != nil {
		t.Fatalf("Inspect by name: %v", err)
	}
	if s.ID != id {
		t.Errorf("ID: got %q, want %q", s.ID, id)
	}
}

func TestInspect_ByPrefix(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "pfx"}, stream)
	if err != nil {
		t.Fatal(err)
	}

	// Use first 5 chars as prefix (IDs are 26-char base32).
	prefix := id[:5]
	s, err := lf.Inspect(ctx, prefix)
	if err != nil {
		t.Fatalf("Inspect by prefix %q: %v", prefix, err)
	}
	if s.ID != id {
		t.Errorf("ID: got %q, want %q", s.ID, id)
	}
}

func TestInspect_NotFound(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	_, err := lf.Inspect(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, snapshot.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "del"}, stream)
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := lf.Delete(ctx, []string{"del"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != id {
		t.Errorf("deleted: got %v, want [%s]", deleted, id)
	}

	// Data dir should be gone.
	if _, err := os.Stat(lf.conf.SnapshotDataDir(id)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("expected data dir to be removed")
	}

	// Inspect should fail.
	if _, err := lf.Inspect(ctx, id); !errors.Is(err, snapshot.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}

	// List should be empty.
	list, _ := lf.List(ctx)
	if len(list) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(list))
	}
}

func TestDelete_ByID(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "delid"}, stream)
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := lf.Delete(ctx, []string{id})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != id {
		t.Errorf("deleted: got %v, want [%s]", deleted, id)
	}
}

func TestDelete_Multiple(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	for _, name := range []string{"m1", "m2", "m3"} {
		stream := makeTar(t, map[string][]byte{"f.txt": []byte(name)})
		if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: name}, stream); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := lf.Delete(ctx, []string{"m1", "m3"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleted) != 2 {
		t.Errorf("expected 2 deleted, got %d", len(deleted))
	}

	// m2 should still exist.
	list, _ := lf.List(ctx)
	if len(list) != 1 || list[0].Name != "m2" {
		t.Errorf("expected only m2 remaining, got %v", list)
	}
}

func TestDelete_DuplicateRefs(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "dedup"}, stream)
	if err != nil {
		t.Fatal(err)
	}

	// Pass the same ref twice — should deduplicate.
	deleted, err := lf.Delete(ctx, []string{id, "dedup"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted (deduped), got %d", len(deleted))
	}
}

func TestDelete_NotFound(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	_, err := lf.Delete(ctx, []string{"nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent ref")
	}
}

func TestCreate_Inspect_Fields(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"cow.raw": []byte("data")})
	cfg := &types.SnapshotConfig{
		ID:           testID(t),
		Name:         "fields",
		Description:  "full field check",
		ImageBlobIDs: map[string]struct{}{"hex1": {}, "hex2": {}},
	}

	id, err := lf.Create(ctx, cfg, stream)
	if err != nil {
		t.Fatal(err)
	}

	s, err := lf.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != id {
		t.Errorf("ID mismatch")
	}
	if s.Name != "fields" {
		t.Errorf("Name: got %q", s.Name)
	}
	if s.Description != "full field check" {
		t.Errorf("Description: got %q", s.Description)
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

func TestDelete_RecreateName(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream1 := makeTar(t, map[string][]byte{"f.txt": []byte("v1")})
	_, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "reuse"}, stream1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := lf.Delete(ctx, []string{"reuse"}); err != nil {
		t.Fatal(err)
	}

	stream2 := makeTar(t, map[string][]byte{"f.txt": []byte("v2")})
	id2, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "reuse"}, stream2)
	if err != nil {
		t.Fatalf("recreate with same name: %v", err)
	}

	s, err := lf.Inspect(ctx, "reuse")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != id2 {
		t.Errorf("expected new ID %q, got %q", id2, s.ID)
	}
}

func TestDataDir(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"cow.raw": []byte("disk")})
	cfg := &types.SnapshotConfig{
		ID:           testID(t),
		Name:         "datadir",
		ImageBlobIDs: map[string]struct{}{"blob1": {}},
		Config: types.Config{
			Image:  "ubuntu:24.04",
			CPU:    2,
			Memory: 1 << 30,
		},
	}

	id, err := lf.Create(ctx, cfg, stream)
	if err != nil {
		t.Fatal(err)
	}

	dataDir, got, _, err := lf.DataDir(ctx, "datadir")
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if dataDir == "" {
		t.Error("expected non-empty dataDir")
	}
	if got.ID != id {
		t.Errorf("ID: got %q, want %q", got.ID, id)
	}
	if got.Name != "datadir" {
		t.Errorf("Name: got %q, want %q", got.Name, "datadir")
	}
	if _, ok := got.ImageBlobIDs["blob1"]; !ok {
		t.Errorf("ImageBlobIDs missing 'blob1': %v", got.ImageBlobIDs)
	}
}

func TestDataDir_NotFound(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	_, _, _, err := lf.DataDir(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, snapshot.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestDataDir_ImageBlobIDsIsolation(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	cfg := &types.SnapshotConfig{
		ID:           testID(t),
		Name:         "iso",
		ImageBlobIDs: map[string]struct{}{"original": {}},
	}
	id, err := lf.Create(ctx, cfg, stream)
	if err != nil {
		t.Fatal(err)
	}

	// Get config via DataDir, mutate the returned ImageBlobIDs.
	_, got1, _, err := lf.DataDir(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got1.ImageBlobIDs["injected"] = struct{}{}

	// Get config again — mutation should NOT be visible.
	_, got2, _, err := lf.DataDir(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got2.ImageBlobIDs["injected"]; ok {
		t.Error("ImageBlobIDs mutation leaked: deep copy is broken")
	}
	if _, ok := got2.ImageBlobIDs["original"]; !ok {
		t.Error("ImageBlobIDs missing 'original' after re-read")
	}
}

func TestRestore_ConfigRoundtrip(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"cow.raw": []byte("disk")})
	cfg := &types.SnapshotConfig{
		ID:           testID(t),
		Name:         "rt",
		Description:  "roundtrip",
		ImageBlobIDs: map[string]struct{}{"deadbeef": {}},
		NICs:         2,
		Config: types.Config{
			Image:   "ubuntu:22.04",
			CPU:     4,
			Memory:  1 << 30, // 1 GiB
			Storage: 10 << 30,
		},
	}

	id, err := lf.Create(ctx, cfg, stream)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, rc, err := lf.Restore(ctx, id)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	rc.Close()

	if got.Name != cfg.Name {
		t.Errorf("Name: got %q, want %q", got.Name, cfg.Name)
	}
	if got.Description != cfg.Description {
		t.Errorf("Description: got %q, want %q", got.Description, cfg.Description)
	}
	if got.Image != cfg.Image {
		t.Errorf("Image: got %q, want %q", got.Image, cfg.Image)
	}
	if _, ok := got.ImageBlobIDs["deadbeef"]; !ok {
		t.Errorf("ImageBlobIDs missing 'deadbeef': %v", got.ImageBlobIDs)
	}
	if got.CPU != cfg.CPU {
		t.Errorf("CPU: got %d, want %d", got.CPU, cfg.CPU)
	}
	if got.Memory != cfg.Memory {
		t.Errorf("Memory: got %d, want %d", got.Memory, cfg.Memory)
	}
	if got.Storage != cfg.Storage {
		t.Errorf("Storage: got %d, want %d", got.Storage, cfg.Storage)
	}
	if got.NICs != cfg.NICs {
		t.Errorf("NICs: got %d, want %d", got.NICs, cfg.NICs)
	}
}

func TestRestore_DataStream(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	wantContent := []byte("hello snapshot data")
	stream := makeTar(t, map[string][]byte{"state.json": wantContent})

	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "ds"}, stream)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, rc, err := lf.Restore(ctx, id)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	defer rc.Close()

	// Read the tar stream and find state.json.
	tr := tar.NewReader(rc)
	found := false
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == "state.json" {
			var buf bytes.Buffer
			if _, err := buf.ReadFrom(tr); err != nil {
				t.Fatalf("read state.json from tar: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), wantContent) {
				t.Errorf("state.json content: got %q, want %q", buf.String(), string(wantContent))
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("state.json not found in restore stream")
	}
}

func TestRestore_CloseWaitsForGoroutine(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "cw"}, stream)
	if err != nil {
		t.Fatal(err)
	}

	_, rc, err := lf.Restore(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	// Close without reading — the background goroutine should still complete
	// without hanging, and Close should not panic.
	if err := rc.Close(); err != nil {
		// A broken pipe or similar error is acceptable here since we didn't
		// consume the stream — but it must not hang or panic.
		t.Logf("Close returned (expected) error: %v", err)
	}
}

func TestRestore_DoubleCloseNoPanic(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: "dc"}, stream)
	if err != nil {
		t.Fatal(err)
	}

	_, rc, err := lf.Restore(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	rc.Close()
	// Second close — must not deadlock or panic (idempotent via sync.Once).
	rc.Close()
}

func TestRestore_ImageBlobIDsIsolation(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	stream := makeTar(t, map[string][]byte{"f.txt": []byte("x")})
	cfg := &types.SnapshotConfig{
		ID:           testID(t),
		Name:         "riso",
		ImageBlobIDs: map[string]struct{}{"orig": {}},
	}
	id, err := lf.Create(ctx, cfg, stream)
	if err != nil {
		t.Fatal(err)
	}

	// Get config via Restore, mutate returned ImageBlobIDs.
	got1, rc1, err := lf.Restore(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rc1.Close()
	got1.ImageBlobIDs["injected"] = struct{}{}

	// Get config again — mutation should NOT be visible.
	got2, rc2, err := lf.Restore(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rc2.Close()
	if _, ok := got2.ImageBlobIDs["injected"]; ok {
		t.Error("ImageBlobIDs mutation leaked through Restore: deep copy is broken")
	}
	if _, ok := got2.ImageBlobIDs["orig"]; !ok {
		t.Error("ImageBlobIDs missing 'orig' after re-read")
	}
}

func TestExportImport_Roundtrip(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	origFiles := map[string][]byte{
		"cow.raw":    []byte("disk data here"),
		"state.json": []byte(`{"cpu":4}`),
	}
	origID := makeExportableSnapshot(t, lf, "export-src", origFiles)

	exportStream, err := lf.Export(ctx, origID)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	defer exportStream.Close()

	importedID, err := lf.Import(ctx, exportStream, "imported-snap", "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if importedID == origID {
		t.Error("imported snapshot should get a new ID")
	}

	// Verify metadata was preserved (except overridden name and ID).
	s, err := lf.Inspect(ctx, importedID)
	if err != nil {
		t.Fatalf("Inspect imported: %v", err)
	}
	if s.Name != "imported-snap" {
		t.Errorf("Name: got %q, want %q", s.Name, "imported-snap")
	}
	if s.Description != "export test" {
		t.Errorf("Description: got %q, want %q", s.Description, "export test")
	}
	if s.CPU != 4 {
		t.Errorf("CPU: got %d, want 4", s.CPU)
	}
	if s.Memory != 1<<30 {
		t.Errorf("Memory: got %d, want %d", s.Memory, int64(1<<30))
	}

	// Verify data files were imported.
	dataDir := lf.conf.SnapshotDataDir(importedID)
	for name, wantContent := range origFiles {
		got, readErr := os.ReadFile(filepath.Join(dataDir, name))
		if readErr != nil {
			t.Errorf("read %s: %v", name, readErr)
			continue
		}
		if !bytes.Equal(got, wantContent) {
			t.Errorf("file %s: got %q, want %q", name, got, wantContent)
		}
	}
}

func TestExportImport_ViaBuffer(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	makeExportableSnapshot(t, lf, "buf-src", map[string][]byte{"data.bin": []byte("hello")})

	// Export to a buffer (simulates writing to a file).
	exportStream, err := lf.Export(ctx, "buf-src")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, exportStream); err != nil {
		t.Fatalf("copy export to buffer: %v", err)
	}
	exportStream.Close()

	// Import from buffer (simulates reading from a file or pipe).
	importedID, err := lf.Import(ctx, &buf, "buf-imported", "new desc")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	s, err := lf.Inspect(ctx, importedID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Name != "buf-imported" {
		t.Errorf("Name: got %q, want %q", s.Name, "buf-imported")
	}
	if s.Description != "new desc" {
		t.Errorf("Description: got %q, want %q", s.Description, "new desc")
	}
}

func TestImport_FromGzipTarReader(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	// Build a gzip-compressed tar archive with snapshot.json + data files.
	wantCfg := types.SnapshotExport{
		Version: 1,
		Config: types.SnapshotConfig{
			Name:        "stream-snap",
			Description: "from reader",
			Config: types.Config{
				CPU:    2,
				Memory: 512 << 20,
			},
		},
	}
	jsonData, err := json.Marshal(wantCfg)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if err := tw.WriteHeader(&tar.Header{
		Name: "snapshot.json", Size: int64(len(jsonData)), Mode: 0o644, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(jsonData); err != nil {
		t.Fatal(err)
	}

	dataContent := []byte("state data")
	if err := tw.WriteHeader(&tar.Header{
		Name: "state.json", Size: int64(len(dataContent)), Mode: 0o644, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(dataContent); err != nil {
		t.Fatal(err)
	}

	tw.Close()
	gw.Close()

	// Import from the in-memory reader.
	importedID, err := lf.Import(ctx, &buf, "", "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	s, err := lf.Inspect(ctx, importedID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Name != "stream-snap" {
		t.Errorf("Name: got %q, want %q", s.Name, "stream-snap")
	}
	if s.CPU != 2 {
		t.Errorf("CPU: got %d, want 2", s.CPU)
	}

	// Verify data file.
	dataDir := lf.conf.SnapshotDataDir(importedID)
	got, err := os.ReadFile(filepath.Join(dataDir, "state.json"))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if !bytes.Equal(got, dataContent) {
		t.Errorf("state.json: got %q, want %q", got, dataContent)
	}
}

func TestImport_CorruptGzipTrailerRejected(t *testing.T) {
	lf := newTestLF(t)

	jsonData, err := json.Marshal(types.SnapshotExport{
		Version: 1,
		Config:  types.SnapshotConfig{Name: "corrupt-snap", Config: types.Config{CPU: 1, Memory: 256 << 20}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "snapshot.json", Size: int64(len(jsonData)), Mode: 0o644, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(jsonData); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	// Flip a bit in the gzip ISIZE trailer: tar extraction still succeeds, so
	// only the drain-to-EOF integrity check can catch it.
	raw := buf.Bytes()
	raw[len(raw)-1] ^= 0xff

	if _, err := lf.Import(t.Context(), bytes.NewReader(raw), "", ""); err == nil {
		t.Fatal("corrupted gzip trailer must fail the import")
	}
}

func TestImport_FromRawTarReader(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	// Build a raw (uncompressed) tar archive with snapshot.json + data files.
	wantCfg := types.SnapshotExport{
		Version: 1,
		Config: types.SnapshotConfig{
			Name:   "raw-snap",
			Config: types.Config{CPU: 8},
		},
	}
	jsonData, err := json.Marshal(wantCfg)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := tw.WriteHeader(&tar.Header{
		Name: "snapshot.json", Size: int64(len(jsonData)), Mode: 0o644, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(jsonData); err != nil {
		t.Fatal(err)
	}

	dataContent := []byte("raw disk data")
	if err := tw.WriteHeader(&tar.Header{
		Name: "cow.raw", Size: int64(len(dataContent)), Mode: 0o644, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(dataContent); err != nil {
		t.Fatal(err)
	}
	tw.Close()

	importedID, err := lf.Import(ctx, &buf, "", "")
	if err != nil {
		t.Fatalf("Import raw tar: %v", err)
	}

	s, err := lf.Inspect(ctx, importedID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Name != "raw-snap" {
		t.Errorf("Name: got %q, want %q", s.Name, "raw-snap")
	}
	if s.CPU != 8 {
		t.Errorf("CPU: got %d, want 8", s.CPU)
	}

	dataDir := lf.conf.SnapshotDataDir(importedID)
	got, err := os.ReadFile(filepath.Join(dataDir, "cow.raw"))
	if err != nil {
		t.Fatalf("read cow.raw: %v", err)
	}
	if !bytes.Equal(got, dataContent) {
		t.Errorf("cow.raw: got %q, want %q", got, dataContent)
	}
}

func TestExportCompressed_ImportRoundtrip(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	origFiles := map[string][]byte{"cow.raw": []byte("compressed roundtrip")}
	makeExportableSnapshot(t, lf, "gz-src", origFiles)

	stream, err := lf.ExportCompressed(ctx, "gz-src")
	if err != nil {
		t.Fatalf("ExportCompressed: %v", err)
	}
	defer stream.Close()

	importedID, err := lf.Import(ctx, stream, "gz-imported", "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	s, err := lf.Inspect(ctx, importedID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Name != "gz-imported" {
		t.Errorf("Name: got %q, want %q", s.Name, "gz-imported")
	}

	dataDir := lf.conf.SnapshotDataDir(importedID)
	for name, want := range origFiles {
		got, readErr := os.ReadFile(filepath.Join(dataDir, name))
		if readErr != nil {
			t.Errorf("read %s: %v", name, readErr)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("file %s: got %q, want %q", name, got, want)
		}
	}
}

func TestExport_ImportRoundtrip(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	origFiles := map[string][]byte{
		"cow.raw":    []byte("disk data"),
		"state.json": []byte(`{"ok":true}`),
	}
	makeExportableSnapshot(t, lf, "raw-export-src", origFiles)

	stream, err := lf.Export(ctx, "raw-export-src")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	defer stream.Close()

	importedID, err := lf.Import(ctx, stream, "raw-imported", "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	s, err := lf.Inspect(ctx, importedID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Name != "raw-imported" {
		t.Errorf("Name: got %q, want %q", s.Name, "raw-imported")
	}

	dataDir := lf.conf.SnapshotDataDir(importedID)
	for name, wantContent := range origFiles {
		got, readErr := os.ReadFile(filepath.Join(dataDir, name))
		if readErr != nil {
			t.Errorf("read %s: %v", name, readErr)
			continue
		}
		if !bytes.Equal(got, wantContent) {
			t.Errorf("file %s: got %q, want %q", name, got, wantContent)
		}
	}
}

func TestImport_InvalidStream(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	// Too short to peek.
	if _, err := lf.Import(ctx, strings.NewReader("x"), "", ""); err == nil {
		t.Fatal("expected error for 1-byte input")
	}

	// Peekable but not a valid tar.
	if _, err := lf.Import(ctx, strings.NewReader("this is not a tar archive"), "", ""); err == nil {
		t.Fatal("expected error for non-tar input")
	}
}

func TestImport_NameOverride(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	makeExportableSnapshot(t, lf, "orig-name", map[string][]byte{"f.txt": []byte("x")})

	exportStream, err := lf.Export(ctx, "orig-name")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	defer exportStream.Close()

	importedID, err := lf.Import(ctx, exportStream, "override-name", "override-desc")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	s, err := lf.Inspect(ctx, importedID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Name != "override-name" {
		t.Errorf("Name: got %q, want %q", s.Name, "override-name")
	}
	if s.Description != "override-desc" {
		t.Errorf("Description: got %q, want %q", s.Description, "override-desc")
	}
}

func TestExportToDir_RoundTrip(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	origFiles := map[string][]byte{
		"cow.raw":    []byte("disk data"),
		"state.json": []byte(`{"cpu":4}`),
	}
	makeExportableSnapshot(t, lf, "to-dir-src", origFiles)

	dst := filepath.Join(t.TempDir(), "exported")
	if err := lf.ExportToDir(ctx, "to-dir-src", dst); err != nil {
		t.Fatalf("ExportToDir: %v", err)
	}

	cfg, err := snapshot.ReadSnapshotEnvelope(dst)
	if err != nil {
		t.Fatalf("ReadSnapshotEnvelope: %v", err)
	}
	if cfg.Name != "to-dir-src" || cfg.CPU != 4 {
		t.Errorf("envelope mismatch: %+v", cfg)
	}

	for name, want := range origFiles {
		got, readErr := os.ReadFile(filepath.Join(dst, name))
		if readErr != nil {
			t.Errorf("read %s: %v", name, readErr)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestExportToDir_RejectNonEmpty(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	makeExportableSnapshot(t, lf, "tdr-non-empty", map[string][]byte{"x": []byte("x")})

	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "preexisting"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := lf.ExportToDir(ctx, "tdr-non-empty", dst)
	if err == nil {
		t.Fatal("want non-empty rejection")
	}
}

// testID generates a random snapshot ID for tests.
func TestCreateSameIDRetryKeepsExistingSnapshot(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	id := testID(t)
	if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: "orig"},
		makeTar(t, map[string][]byte{"x": []byte("1")})); err != nil {
		t.Fatalf("first create: %v", err)
	}

	if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: "retry"},
		makeTar(t, map[string][]byte{"x": []byte("2")})); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("same-ID retry must be rejected, got: %v", err)
	}

	rec, err := lf.lookupRecord(ctx, id, false)
	if err != nil || rec.Name != "orig" {
		t.Fatalf("original record must survive the retry: rec=%+v err=%v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(rec.DataDir, "x")); err != nil {
		t.Fatalf("original data dir must survive the retry: %v", err)
	}
}

func TestDeleteRejectsLeasedSnapshot(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()
	id := testID(t)
	if _, err := lf.Create(ctx, &types.SnapshotConfig{ID: id, Name: "leased"},
		makeTar(t, map[string][]byte{"x": []byte("1")})); err != nil {
		t.Fatalf("create: %v", err)
	}

	release, err := lf.acquireReadLease(ctx, id)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if _, err := lf.Delete(ctx, []string{id}); err == nil || !strings.Contains(err.Error(), "in use") {
		release()
		t.Fatalf("delete must fail while a reader holds the lease, got: %v", err)
	}
	if _, err := lf.lookupRecord(ctx, id, false); err != nil {
		release()
		t.Fatalf("record must survive the refused delete: %v", err)
	}
	release()
	if _, err := lf.Delete(ctx, []string{id}); err != nil {
		t.Fatalf("delete after release: %v", err)
	}
}

func testID(t *testing.T) string {
	t.Helper()
	return utils.GenerateID()
}

// newTestLF creates a LocalFile backed by a temp directory.
func newTestLF(t *testing.T) *LocalFile {
	t.Helper()
	return newTestLFWithRecorder(t, metering.NopRecorder{})
}

// newTestLFWithRecorder lets tests inject a CaptureRecorder for emit assertions.
func newTestLFWithRecorder(t *testing.T, rec metering.Recorder) *LocalFile {
	t.Helper()
	dir := t.TempDir()
	conf := &config.Config{RootDir: dir}
	lf, err := New(conf, rec, newTestMetaStore(t, conf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return lf
}

// writeCaptureDir stages a capture directory (like a hypervisor's snapshot tmp dir) for CreateFromDir.
func writeCaptureDir(t *testing.T, dir string, files map[string][]byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// readDirFiles reads a directory's regular files into a name→content map.
func readDirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

// makeTar builds a tar archive in memory from a map of name→content.
func makeTar(t *testing.T, files map[string][]byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Size:     int64(len(data)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	return &buf
}

func kinds(entries []metering.Entry) []metering.Kind {
	out := make([]metering.Kind, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

// makeExportableSnapshot creates a snapshot with data files and returns its name.
func makeExportableSnapshot(t *testing.T, lf *LocalFile, name string, files map[string][]byte) string {
	t.Helper()
	ctx := t.Context()
	stream := makeTar(t, files)
	cfg := &types.SnapshotConfig{
		ID:           testID(t),
		Name:         name,
		Description:  "export test",
		ImageBlobIDs: map[string]struct{}{"blob1": {}},
		NICs:         2,
		Config: types.Config{
			Image:   "ubuntu:24.04",
			CPU:     4,
			Memory:  1 << 30,
			Storage: 10 << 30,
		},
	}
	id, err := lf.Create(ctx, cfg, stream)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// newTestMetaStore opens the snapshot namespace over conf for tests.
func newTestMetaStore(t *testing.T, conf *config.Config) *metajson.Store {
	t.Helper()
	store, err := metajson.Open(MetaNamespace(conf))
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
