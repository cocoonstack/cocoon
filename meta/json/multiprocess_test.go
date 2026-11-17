package json

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/contracttest"
)

const (
	mpOps = 15

	inverseWorkers = 8
	inverseOps     = 10
)

func TestMultiProcessCorrectness(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	workers := contracttest.StormWorkers()
	cmds := contracttest.SpawnWorkers(t, workers, "TestMultiProcessWorker", dir, func(int) []string {
		return []string{fmt.Sprintf("META_MP_OPS=%d", mpOps)}
	})
	contracttest.WaitWorkers(t, cmds, "failed")

	s := newStore(t, dir, "alpha")
	if recs, err := mustGet(t, s, "alpha", "w0-op0"); err != nil || recs == nil {
		t.Fatalf("spot check: %v %v", recs, err)
	}
	if total, want := contracttest.CountRaw(t, s, "alpha", "records"), workers*mpOps; total != want {
		t.Fatalf("lost writes: %d records, want %d", total, want)
	}
}

func TestMultiProcessWorker(t *testing.T) {
	dir, worker := contracttest.HelperEnv(t)
	ops, err := strconv.Atoi(os.Getenv("META_MP_OPS"))
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int]("alpha", "records")
	ctx := t.Context()
	for i := range ops {
		id := fmt.Sprintf("w%s-op%d", worker, i)
		if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
			return c.Insert(ctx, w, id, new(map[string]int{"n": i}))
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
}

func TestInverseScopeNoDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	scopes := []string{"alpha:beta", "beta:alpha"}
	cmds := contracttest.SpawnWorkers(t, 2*inverseWorkers, "TestInverseScopeWorker", dir, func(w int) []string {
		return []string{"META_MP_SCOPE=" + scopes[w%2]}
	})
	contracttest.WaitWorkers(t, cmds, "failed (deadlock or error)")
	s := newStore(t, dir, "alpha", "beta")
	for _, ns := range []string{"alpha", "beta"} {
		if total, want := contracttest.CountRaw(t, s, ns, "records"), inverseWorkers*inverseOps; total != want {
			t.Fatalf("%s lost writes: %d records, want %d", ns, total, want)
		}
	}
}

func TestInverseScopeWorker(t *testing.T) {
	dir, worker := contracttest.HelperEnv(t)
	write, read, _ := strings.Cut(os.Getenv("META_MP_SCOPE"), ":")
	s := newStore(t, dir, "alpha", "beta")
	ctx := t.Context()
	c := meta.NewCollection[map[string]int](write, "records")
	peer := meta.NewCollection[map[string]int](read, "records")
	for i := range inverseOps {
		id := fmt.Sprintf("w%s-op%d", worker, i)
		if err := s.Update(ctx, meta.Scope{Write: write, Read: []string{read}}, meta.CommitDurable, func(w meta.Writer) error {
			seen := 0
			if err := peer.Scan(ctx, w, func(string, *map[string]int) error {
				seen++
				return nil
			}); err != nil {
				return err
			}
			return c.Insert(ctx, w, id, new(map[string]int{"peer": seen}))
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
}

func TestAckedDurableSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	acked := contracttest.CollectAcks(t, contracttest.WorkerCmd("TestAckWorker", "META_MP_DIR="+dir), 25)
	if len(acked) == 0 {
		t.Fatal("worker produced no ACKs")
	}

	s := newStore(t, dir, "alpha")
	for _, id := range acked {
		if _, err := mustGet(t, s, "alpha", id); err != nil {
			t.Fatalf("acked durable write %s lost after SIGKILL: %v", id, err)
		}
	}
}

func TestAckWorker(t *testing.T) {
	dir, _ := contracttest.HelperEnv(t)
	s := newStore(t, dir, "alpha")
	ctx := t.Context()
	c := meta.NewCollection[map[string]int]("alpha", "records")
	for i := range 1000 {
		id := fmt.Sprintf("op%d", i)
		if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
			return c.Insert(ctx, w, id, new(map[string]int{"n": i}))
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		fmt.Printf("ACK %s\n", id)
	}
}

func TestEventsExternalProcess(t *testing.T) {
	contracttest.ExternalProcessEvents(t, func(t *testing.T, dir string) meta.Store { return newStore(t, dir, "alpha") }, "TestEventsWriterWorker")
}

func TestEventsWriterWorker(t *testing.T) {
	dir, _ := contracttest.HelperEnv(t)
	s := newStore(t, dir, "alpha")
	ctx := t.Context()
	c := meta.NewCollection[map[string]int]("alpha", "records")
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "ext", new(map[string]int{"ext": 1}))
	}); err != nil {
		t.Fatal(err)
	}
}
