package sqlite

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/contracttest"
	"github.com/cocoonstack/cocoon/utils"
)

func TestBackupFidelity(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "vms")
	put := func(id, val string) {
		if err := putRecord(ctx, s, "vms", id, val); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}

	put("marker", `{"v":1}`)
	stop := make(chan struct{})
	started := make(chan struct{})
	var churnErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			err := putRecord(ctx, s, "vms", "churn", `{"i":`+string(rune('0'+i%10))+`}`)
			if i == 0 {
				close(started)
			}
			if err != nil {
				churnErr = err
				return
			}
		}
	})
	<-started
	dest := filepath.Join(dir, "backup", "meta.db")
	backupErr := Backup(ctx, filepath.Join(dir, DBFileName), dest)
	close(stop)
	wg.Wait()
	if churnErr != nil {
		t.Fatalf("churn write: %v", churnErr)
	}
	if backupErr != nil {
		t.Fatalf("backup: %v", backupErr)
	}
	if raw, ok := backupGet(t, dest, "marker"); !ok || raw != `{"v":1}` {
		t.Fatalf("marker missing from backup: %q ok=%v", raw, ok)
	}

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
	if err := putRecord(ctx, s, "vms", "id1", `{"v":1}`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := filepath.Join(dir, "backup", "meta.db")
	if err := Backup(ctx, filepath.Join(dir, DBFileName), dest); err != nil {
		t.Fatalf("publish first backup: %v", err)
	}
	t.Cleanup(func() { testBackupStep = nil })

	errCrash := errors.New("injected crash")
	for _, step := range []string{"vacuumed", "verified", "synced", "renamed"} {
		testBackupStep = func(at string) error {
			if at == step {
				return errCrash
			}
			return nil
		}
		err := Backup(ctx, filepath.Join(dir, DBFileName), dest)
		if !errors.Is(err, errCrash) {
			t.Fatalf("step %s: want injected crash, got %v", step, err)
		}
		if step != "renamed" {
			if raw, ok := backupGet(t, dest, "id1"); !ok || raw != `{"v":1}` {
				t.Fatalf("step %s: published backup damaged: %q ok=%v", step, raw, ok)
			}
		}

		testBackupStep = nil
		if err := Backup(ctx, filepath.Join(dir, DBFileName), dest); err != nil {
			t.Fatalf("rerun after %s: %v", step, err)
		}
	}
}

func TestBackupRefusals(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, DBFileName)
	requireErrContains(t, Backup(t.Context(), missing, filepath.Join(dir, "out.db")), "no sqlite store")
	if utils.FileExists(missing) {
		t.Fatal("backup created the missing source")
	}
	if err := os.WriteFile(missing, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	requireErrContains(t, Backup(t.Context(), missing, filepath.Join(dir, "out.db")), "uninitialized store")
	if err := Init(t.Context(), missing, testDecls()...); err != nil {
		t.Fatalf("init over crashed file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	requireErrContains(t, Backup(t.Context(), missing, filepath.Join(dir, "out.db")), "conversion is in flight")
}

func TestBusyCtxDeadline(t *testing.T) {
	dir := t.TempDir()
	s1 := newStore(t, dir, "vms")
	s2 := newStore(t, dir, "vms")

	defer contracttest.HoldWriter(t, s1, meta.Scope{Write: "vms"}, func(meta.Writer) error { return nil })()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s2.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(meta.Writer) error { return nil })
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("returned after %s; the ctx bound did not hold", elapsed)
	}
}

func TestBackupConcurrentSameDest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		dir := t.TempDir()
		s := newStore(t, dir, "vms")
		if err := putRecord(ctx, s, "vms", "id1", `{"v":1}`); err != nil {
			t.Fatalf("seed: %v", err)
		}
		dest := filepath.Join(dir, "backup", "meta.db")
		done := make(chan error, 1)
		launched := false
		testBackupStep = func(at string) error {
			if at == "vacuumed" && !launched {
				launched = true
				go func() { done <- Backup(ctx, filepath.Join(dir, DBFileName), dest) }()
				synctest.Wait()
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
	})
}

func backupGet(t *testing.T, dest, id string) (string, bool) {
	t.Helper()
	b, err := Open(dest, Namespace{Name: "vms", Tables: []string{"records", "names", "tombstones"}})
	if err != nil {
		t.Fatalf("open backup %s: %v", dest, err)
	}
	defer b.Close()
	var raw jsontext.Value
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
