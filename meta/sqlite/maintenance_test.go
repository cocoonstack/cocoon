package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta"
)

func TestBackupFidelity(t *testing.T) {
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
		t.Fatalf("backup: %v", err)
	}
	b, err := Open(dest, Namespace{Name: "vms", Tables: []string{"records", "names", "tombstones"}})
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer b.Close() //nolint:errcheck
	err = b.View(ctx, []string{"vms"}, func(r meta.Reader) error {
		raw, ok, err := r.GetRaw(ctx, "vms", "records", "id1")
		if err != nil || !ok || string(raw) != `{"v":1}` {
			t.Fatalf("backup content: %s ok=%v err=%v", raw, ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view backup: %v", err)
	}

	if err := Backup(ctx, filepath.Join(dir, DBFileName), dest); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("want overwrite refusal, got %v", err)
	}
}

func TestBusyCtxDeadline(t *testing.T) {
	dir := t.TempDir()
	s1 := newStore(t, dir, "vms")
	s2 := newStore(t, dir, "vms")

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = s1.Update(context.Background(), meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
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
