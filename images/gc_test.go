package images

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gofrsflock "github.com/gofrs/flock"
	"github.com/projecteru2/core/log"
	coretypes "github.com/projecteru2/core/types"

	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/meta/tombstone"
)

var testImageTables = metajson.TableCodec{Specs: []metajson.TableSpec{
	{Key: "images", Table: TableRecords},
	{Key: "tombstones", Table: tombstone.TableName, Optional: true},
}}

func TestGCCollectSkipsRepublishedBlob(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	engine, err := metajson.Open(metajson.Namespace{Name: "images_test", FilePath: filepath.Join(dir, "images.json"), LockPath: filepath.Join(dir, "images.lock"), Codec: testImageTables})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	store := NewMetaStore[gcTestEntry](engine, "images_test")

	var removed []string
	mod := BuildGCModule(GCModuleConfig[gcTestEntry]{
		Name:     "test",
		Store:    store,
		LockPath: func(hex string) string { return filepath.Join(dir, hex+".lock") },
		ReadRefs: func(m map[string]*gcTestEntry) map[string]struct{} {
			refs := map[string]struct{}{}
			for _, e := range m {
				refs[e.Digest] = struct{}{}
			}
			return refs
		},
		ScanDisk: func() ([]string, error) { return []string{"deadbeef"}, nil },
		Removers: []func(string) error{func(hex string) error { removed = append(removed, hex); return nil }},
		TempDir:  dir,
	})

	if err := store.Update(ctx, func(idx *Index[gcTestEntry]) error {
		idx.Images["ref1"] = &gcTestEntry{Digest: "deadbeef"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Collect(ctx, []string{"deadbeef"}, ImageGCSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("republished blob removed: %v", removed)
	}

	if err := store.Update(ctx, func(idx *Index[gcTestEntry]) error {
		delete(idx.Images, "ref1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Collect(ctx, []string{"deadbeef"}, ImageGCSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "deadbeef" {
		t.Fatalf("unreferenced blob not collected: %v", removed)
	}
}

func TestLegacyDifferentialTrace(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	baseline, err := os.ReadFile("testdata/legacy-images.baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile("testdata/legacy-images.after.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "images.json")
	if err := os.WriteFile(path, baseline, 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := metajson.Open(metajson.Namespace{
		Name: "images_test", FilePath: path, LockPath: filepath.Join(dir, "images.lock"), Codec: testImageTables,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	store := NewMetaStore[fixtureEntry](engine, "images_test")

	if err := store.Update(ctx, func(*Index[fixtureEntry]) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(baseline) {
		t.Fatalf("round-trip not byte-identical:\n got: %s\nwant: %s", got, baseline)
	}

	t2 := time.Date(2026, 7, 3, 12, 45, 0, 0, time.UTC)
	if err := store.Update(ctx, func(idx *Index[fixtureEntry]) error {
		delete(idx.Images, "docker.io/library/a:1")
		idx.Images["docker.io/library/c:3"] = &fixtureEntry{Ref: "docker.io/library/c:3", Digest: "sha256:cccc", Size: 200, CreatedAt: t2}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(after) {
		t.Fatalf("differential trace diverged:\n got: %s\nwant: %s", got, after)
	}
}

func TestGCCollectRespectsExternalPins(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	engine, err := metajson.Open(metajson.Namespace{Name: "images_pins", FilePath: filepath.Join(dir, "images.json"), LockPath: filepath.Join(dir, "images.lock"), Codec: testImageTables})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	store := NewMetaStore[gcTestEntry](engine, "images_pins")

	pins := map[string]struct{}{"deadbeef": {}}
	var removed []string
	mod := BuildGCModule(GCModuleConfig[gcTestEntry]{
		Name:     "test",
		Store:    store,
		LockPath: func(hex string) string { return filepath.Join(dir, hex+".lock") },
		ReadRefs: func(map[string]*gcTestEntry) map[string]struct{} { return map[string]struct{}{} },
		ScanDisk: func() ([]string, error) { return []string{"deadbeef"}, nil },
		Removers: []func(string) error{func(hex string) error { removed = append(removed, hex); return nil }},
		TempDir:  dir,
		PinnedElsewhere: func(context.Context) (map[string]struct{}, error) {
			return pins, nil
		},
	})

	if err := mod.Collect(ctx, []string{"deadbeef"}, ImageGCSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("externally pinned blob removed: %v", removed)
	}

	pins = map[string]struct{}{}
	if err := mod.Collect(ctx, []string{"deadbeef"}, ImageGCSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "deadbeef" {
		t.Fatalf("unpinned blob not collected: %v", removed)
	}
}

func TestPinBlobs(t *testing.T) {
	cfg := &BaseConfig{RootDir: t.TempDir(), Subdir: "oci", BlobExt: ".erofs"}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.BlobPath("deadbeef"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := PinBlobs(cfg, map[string]struct{}{"deadbeef": {}})
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	fl := gofrsflock.New(cfg.BlobLockPath("deadbeef"))
	if ok, _ := fl.TryLock(); ok {
		t.Fatal("digest lock not held during pin")
	}
	release()
	if ok, err := fl.TryLock(); err != nil || !ok {
		t.Fatalf("digest lock not released: %v %v", ok, err)
	}
	_ = fl.Close()

	if _, err := PinBlobs(cfg, map[string]struct{}{"cafebabe": {}}); err == nil {
		t.Fatal("pin of a missing blob must fail")
	}
}

func TestGCCollectLogsUnreferencedReason(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gc.log")
	if err := log.SetupLog(ctx, &coretypes.ServerLogConfig{Level: "info", Filename: logPath}, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.SetupLog(ctx, &coretypes.ServerLogConfig{Level: "info"}, "") })

	engine, err := metajson.Open(metajson.Namespace{Name: "images_reason", FilePath: filepath.Join(dir, "images.json"), LockPath: filepath.Join(dir, "images.lock"), Codec: testImageTables})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	mod := BuildGCModule(GCModuleConfig[gcTestEntry]{
		Name:     "oci",
		Store:    NewMetaStore[gcTestEntry](engine, "images_reason"),
		LockPath: func(hex string) string { return filepath.Join(dir, hex+".lock") },
		ReadRefs: func(map[string]*gcTestEntry) map[string]struct{} { return map[string]struct{}{} },
		ScanDisk: func() ([]string, error) { return []string{"deadbeef"}, nil },
		Removers: []func(string) error{func(string) error { return nil }},
		TempDir:  dir,
	})
	if err := mod.Collect(ctx, []string{"deadbeef"}, ImageGCSnapshot{}); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "collected blob=deadbeef reason=unreferenced") {
		t.Fatalf("gc.oci log: got %s, want a collected line with reason=unreferenced", out)
	}
}

type gcTestEntry struct {
	Digest string `json:"digest"`
}

type fixtureEntry struct {
	Ref       string    `json:"ref"`
	Digest    string    `json:"manifest_digest"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}
