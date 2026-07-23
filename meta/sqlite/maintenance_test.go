package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta"
)

func TestBackupFidelity(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "vms")
	put := func(id, val string) {
		err := s.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
			return w.PutRaw(ctx, "vms", "records", id, json.RawMessage(val), false)
		})
		if err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	// Pre-snapshot marker: acked before backup, MUST be captured even while
	// a writer keeps the WAL non-empty (§9 backup fidelity).
	put("marker", `{"v":1}`)
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				put("churn", `{"i":`+string(rune('0'+i%10))+`}`)
			}
		}
	}()
	dest := filepath.Join(dir, "backup", "meta.db")
	if err := Backup(ctx, filepath.Join(dir, DBFileName), dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	close(stop)
	if raw, ok := backupGet(t, dest, "marker"); !ok || raw != `{"v":1}` {
		t.Fatalf("marker missing from backup: %q ok=%v", raw, ok)
	}

	// Replacement: a second backup overwrites and carries new state.
	put("marker", `{"v":2}`)
	if err := Backup(ctx, filepath.Join(dir, DBFileName), dest); err != nil {
		t.Fatalf("replace backup: %v", err)
	}
	if raw, _ := backupGet(t, dest, "marker"); raw != `{"v":2}` {
		t.Fatalf("replacement backup content: %q", raw)
	}
}

func TestBackupCrashSteps(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "vms")
	err := s.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "vms", "records", "id1", json.RawMessage(`{"v":1}`), false)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := filepath.Join(dir, "backup", "meta.db")
	if err := Backup(ctx, filepath.Join(dir, DBFileName), dest); err != nil {
		t.Fatalf("publish first backup: %v", err)
	}

	errCrash := errors.New("injected crash")
	for _, step := range []string{"vacuumed", "verified", "synced", "renamed"} {
		testBackupStep = func(at string) error {
			if at == step {
				return errCrash
			}
			return nil
		}
		err := Backup(ctx, filepath.Join(dir, DBFileName), dest)
		if step != "renamed" {
			// Crash before rename: the published backup must be intact.
			if !errors.Is(err, errCrash) {
				t.Fatalf("step %s: want injected crash, got %v", step, err)
			}
			if raw, ok := backupGet(t, dest, "id1"); !ok || raw != `{"v":1}` {
				t.Fatalf("step %s: published backup damaged: %q ok=%v", step, raw, ok)
			}
		} else if !errors.Is(err, errCrash) {
			t.Fatalf("step renamed: want injected crash, got %v", err)
		}
		// Rerun after every crash point must succeed (stale tmp cleared).
		testBackupStep = nil
		if err := Backup(ctx, filepath.Join(dir, DBFileName), dest); err != nil {
			t.Fatalf("rerun after %s: %v", step, err)
		}
	}
}

func TestBusyCtxDeadline(t *testing.T) {
	dir := t.TempDir()
	s1 := newStore(t, dir, "vms")
	s2 := newStore(t, dir, "vms")

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = s1.Update(t.Context(), meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s2.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error { return nil })
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("returned after %s; the ctx bound did not hold", elapsed)
	}
}

// TestBackupConcurrentSameDest pins the flock serialization: without it, a
// concurrent run's stale-tmp cleanup yanks the first run's tmp mid-verify
// and publishes an empty backup with a nil error.
func TestBackupConcurrentSameDest(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "vms")
	err := s.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "vms", "records", "id1", json.RawMessage(`{"v":1}`), false)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := filepath.Join(dir, "backup", "meta.db")
	done := make(chan error, 1)
	launched := false
	testBackupStep = func(at string) error {
		if at == "vacuumed" && !launched {
			launched = true
			go func() { done <- Backup(ctx, filepath.Join(dir, DBFileName), dest) }()
			time.Sleep(150 * time.Millisecond)
		}
		return nil
	}
	defer func() { testBackupStep = nil }()
	if err := Backup(ctx, filepath.Join(dir, DBFileName), dest); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if raw, ok := backupGet(t, dest, "id1"); !ok || raw != `{"v":1}` {
		t.Fatalf("published backup invalid after concurrent runs: %q ok=%v", raw, ok)
	}
}

func backupGet(t *testing.T, dest, id string) (string, bool) {
	t.Helper()
	b, err := Open(dest, Namespace{Name: "vms", Tables: []string{"records", "names", "tombstones"}})
	if err != nil {
		t.Fatalf("open backup %s: %v", dest, err)
	}
	defer b.Close() //nolint:errcheck
	var raw json.RawMessage
	var ok bool
	err = b.View(t.Context(), []string{"vms"}, func(r meta.Reader) error {
		raw, ok, err = r.GetRaw(t.Context(), "vms", "records", id)
		return err
	})
	if err != nil {
		t.Fatalf("view backup: %v", err)
	}
	return string(raw), ok
}
