package localfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	metajson "github.com/cocoonstack/cocoon/meta/json"
)

func TestLegacyDifferentialTrace(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	baseline, err := os.ReadFile("testdata/legacy-snapshots.baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile("testdata/legacy-snapshots.after.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "snapshots.json")
	if err := os.WriteFile(path, baseline, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := metajson.Open(metajson.Namespace{
		Name: NamespaceName, FilePath: path, LockPath: filepath.Join(dir, "snapshots.lock"), Codec: testSnapTables,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lf := &LocalFile{meta: store}

	if err := lf.update(ctx, func(*snapTx) error { return nil }); err != nil {
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
	if err := lf.update(ctx, func(tx *snapTx) error {
		p, err := tx.Get("SNAP333333333333")
		if err != nil {
			return err
		}
		if p == nil {
			return errors.New("fixture record SNAP333333333333 missing")
		}
		p.Pending = false
		p.SizeBytes = 8192
		p.LastAccessedAt = t2
		if err := tx.Put("SNAP333333333333", p); err != nil {
			return err
		}
		n, err := tx.Get("SNAP111111111111")
		if err != nil {
			return err
		}
		if n == nil {
			return errors.New("fixture record SNAP111111111111 missing")
		}
		n.LastAccessedAt = t2
		if err := tx.Put("SNAP111111111111", n); err != nil {
			return err
		}
		return tx.Del("SNAP222222222222")
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
