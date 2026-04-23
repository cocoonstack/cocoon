package localfile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// helpers

// testID generates a random snapshot ID for tests.
func testID(t *testing.T) string {
	t.Helper()
	return utils.GenerateID()
}

// newTestLF creates a LocalFile backed by a temp directory.
func newTestLF(t *testing.T) *LocalFile {
	t.Helper()
	dir := t.TempDir()
	lf, err := New(&config.Config{RootDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return lf
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

// New

func TestNew(t *testing.T) {
	dir := t.TempDir()
	lf, err := New(&config.Config{RootDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if lf == nil {
		t.Fatal("expected non-nil LocalFile")
	}
}

func TestNew_NilConfig(t *testing.T) {
	_, err := New(nil)
	if err == nil {
		t.Fatal("expected error for nil config")
	}
}

// Create

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

// List

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

// Inspect

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

// Delete

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

	var ids []string
	for _, name := range []string{"m1", "m2", "m3"} {
		stream := makeTar(t, map[string][]byte{"f.txt": []byte(name)})
		id, err := lf.Create(ctx, &types.SnapshotConfig{ID: testID(t), Name: name}, stream)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
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

// Create → Inspect round trip verifies timestamps and fields.

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

// Delete then recreate with same name should succeed.

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

// DataDir

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

	dataDir, got, err := lf.DataDir(ctx, "datadir")
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

	_, _, err := lf.DataDir(ctx, "nonexistent")
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
	_, got1, err := lf.DataDir(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got1.ImageBlobIDs["injected"] = struct{}{}

	// Get config again — mutation should NOT be visible.
	_, got2, err := lf.DataDir(ctx, id)
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

// Restore

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

	// First close — should work normally.
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

// Export → Import roundtrip

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

	// snapshot.json entry.
	if err := tw.WriteHeader(&tar.Header{
		Name: "snapshot.json", Size: int64(len(jsonData)), Mode: 0o644, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(jsonData); err != nil {
		t.Fatal(err)
	}

	// data file entry.
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

func TestImport_FromRawTarReader(t *testing.T) {
	lf := newTestLF(t)
	ctx := t.Context()

	// Build a raw (uncompressed) tar archive with snapshot.json + data files.
	wantCfg := types.SnapshotExport{
		Version: 1,
		Config: types.SnapshotConfig{
			Name:           "raw-snap",
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
