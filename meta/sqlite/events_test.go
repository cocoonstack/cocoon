package sqlite

import (
	"fmt"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/contracttest"
)

func TestEventsExternalProcess(t *testing.T) {
	contracttest.ExternalProcessEvents(t, func(t *testing.T, dir string) meta.Store { return newStore(t, dir, "alpha") }, "TestEventsWriterWorker")
}

func TestEventsWriterWorker(t *testing.T) {
	dir, _ := contracttest.HelperEnv(t)
	s := newStore(t, dir, "alpha")
	if err := putRecord(t.Context(), s, "alpha", "ext", `{}`); err != nil {
		t.Fatal(err)
	}
}

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
		if err := putRecord(ctx, s, "alpha", fmt.Sprintf("churn%d", i), `{}`); err != nil {
			t.Fatal(err)
		}
		contracttest.WaitEvent(t, ch, 8*time.Second, fmt.Sprintf("commit %d under pool churn", i))
	}
}

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
	if err := putRecord(ctx, s, "alpha", "silent", `{}`); err != nil {
		t.Fatal(err)
	}
	contracttest.WaitEvent(t, ch, 8*time.Second, "poll fallback after severed watch")
}
