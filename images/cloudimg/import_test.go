package cloudimg

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/images"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	"github.com/cocoonstack/cocoon/utils"
)

func TestImportPreservesSourceAcrossCacheGC(t *testing.T) {
	for _, tt := range []struct {
		name        string
		cached      bool
		collect     bool
		wantCommits int
	}{
		{name: "cold import", wantCommits: 1},
		{name: "cache hit", cached: true, wantCommits: 1},
		{name: "cache collected before publication", cached: true, collect: true, wantCommits: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newImportTestBackend(t)
			ctx := t.Context()
			payload := make([]byte, 32)
			copy(payload, utils.Qcow2Magic)
			binary.BigEndian.PutUint32(payload[4:8], 3)
			digest := sha256Hex(payload)
			source := filepath.Join(t.TempDir(), "original.qcow2")
			if err := os.WriteFile(source, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			if tt.cached {
				if err := os.WriteFile(c.conf.BlobPath(digest), payload, 0o444); err != nil {
					t.Fatal(err)
				}
			}
			m := c.GCModule()
			snap, err := m.ReadDB(ctx)
			if err != nil {
				t.Fatal(err)
			}
			commits := 0
			tracker := progress.NewTracker(func(e cloudimgProgress.Event) {
				if e.Phase != cloudimgProgress.PhaseCommit {
					return
				}
				commits++
				if tt.cached && commits == 1 {
					entries, err := os.ReadDir(c.conf.TempDir())
					if err != nil || len(entries) != 0 {
						t.Fatalf("cache hit copied input: entries=%v err=%v", entries, err)
					}
				}
				if tt.collect && commits == 1 {
					if err := m.Collect(ctx, []string{digest}, snap); err != nil {
						t.Fatal(err)
					}
					if utils.ValidFile(c.conf.BlobPath(digest)) {
						t.Fatal("unreferenced cache blob survived GC")
					}
				}
			})
			if err := c.Import(ctx, "imported", tracker, source); err != nil {
				t.Fatalf("import: %v", err)
			}
			if commits != tt.wantCommits {
				t.Errorf("commit attempts=%d, want %d", commits, tt.wantCommits)
			}
			after, err := os.Stat(source)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("input file moved or replaced: %v", err)
			}
			for _, path := range []string{source, c.conf.BlobPath(digest)} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, payload) {
					t.Errorf("file %s content changed: %v", path, err)
				}
			}
			if err := m.Collect(ctx, []string{digest}, snap); err != nil {
				t.Fatal(err)
			}
			if !utils.ValidFile(c.conf.BlobPath(digest)) {
				t.Fatal("published image lost its blob to GC")
			}
			if img, err := c.Inspect(ctx, "imported"); err != nil || img.ID != images.NewDigest(digest).String() {
				t.Fatalf("published image=%+v err=%v", img, err)
			}
		})
	}
}

func TestCommitRepublishesABlobCollectedAfterTheEntryCheck(t *testing.T) {
	c := newImportTestBackend(t)
	ctx := t.Context()
	payload := make([]byte, 32)
	copy(payload, utils.Qcow2Magic)
	binary.BigEndian.PutUint32(payload[4:8], 3)
	digest := sha256Hex(payload)
	if err := os.WriteFile(c.conf.BlobPath(digest), payload, 0o444); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(c.conf.TempDir(), "pull-download.qcow2")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := progress.NewTracker(func(e cloudimgProgress.Event) {
		if e.Phase == cloudimgProgress.PhaseCommit {
			if err := os.Remove(c.conf.BlobPath(digest)); err != nil {
				t.Fatal(err)
			}
		}
	})

	if err := commit(ctx, c.conf, c.store, "pulled", tracker, source, digest); err != nil {
		t.Fatalf("commit after the cached blob was collected: %v", err)
	}
	got, err := os.ReadFile(c.conf.BlobPath(digest))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("blob not republished from the download: %v", err)
	}
	if img, err := c.Inspect(ctx, "pulled"); err != nil || img.ID != images.NewDigest(digest).String() {
		t.Fatalf("published image=%+v err=%v", img, err)
	}
}

func newImportTestBackend(t *testing.T) *CloudImg {
	t.Helper()
	root := t.TempDir()
	cfg := NewConfig(root, 1)
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := metajson.Open(metajson.Namespace{
		Name: NamespaceName, FilePath: cfg.IndexFile(), LockPath: cfg.IndexLock(),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{{Key: "images", Table: images.TableRecords}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c, err := New(t.Context(), root, 1, store)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
