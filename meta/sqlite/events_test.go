package sqlite

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta"
)

func waitEvent(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("no event within %s (%s)", within, what)
	}
}

// TestEventsExternalProcess is §9's cross-process signal gate: a commit by
// ANOTHER process must reach this process's subscribers.
func TestEventsExternalProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	ch, release, err := s.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	cmd := exec.Command(os.Args[0], "-test.run=TestEventsWriterWorker$", "-test.count=1") //nolint:gosec
	cmd.Env = append(os.Environ(), "META_MP_DIR="+dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("external writer: %v", err)
	}
	waitEvent(t, ch, 8*time.Second, "external-process commit")
}

// TestEventsWriterWorker is the helper-process body.
func TestEventsWriterWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	s := newStore(t, dir, "alpha")
	ctx := t.Context()
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "alpha", "records", "ext", json.RawMessage(`{}`), false)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestEventsPoolChurn: reader-pool connections cycling under load must not
// blind the notifier — its pinned connection is untouched by the pool (§7).
func TestEventsPoolChurn(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	ch, release, err := s.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx := t.Context()
	for i := range 5 {
		for range 20 {
			if err := s.View(ctx, []string{"alpha"}, func(r meta.Reader) error {
				_, _, gerr := r.GetRaw(ctx, "alpha", "records", "x")
				return gerr
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
			return w.PutRaw(ctx, "alpha", "records", fmt.Sprintf("churn%d", i), json.RawMessage(`{}`), false)
		}); err != nil {
			t.Fatal(err)
		}
		waitEvent(t, ch, 8*time.Second, fmt.Sprintf("commit %d under pool churn", i))
	}
}

// TestEventsSeveredWatchPollFallback: a lost fsnotify watch must degrade to
// the change-token-confirmed safety poll, not a permanently missed change.
func TestEventsSeveredWatchPollFallback(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	ch, release, err := s.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.notifier.watcher.Remove(dir); err != nil {
		t.Fatalf("sever watch: %v", err)
	}
	ctx := t.Context()
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "alpha", "records", "silent", json.RawMessage(`{}`), false)
	}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ch, 8*time.Second, "poll fallback after severed watch")
}
