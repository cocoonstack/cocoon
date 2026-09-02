package json

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

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
	workers := stormWorkers()
	cmds := contracttest.SpawnWorkers(t, workers, "TestMultiProcessWorker", func(w int) []string {
		return []string{"META_MP_DIR=" + dir, "META_MP_WORKER=" + strconv.Itoa(w), fmt.Sprintf("META_MP_OPS=%d", mpOps)}
	})
	contracttest.WaitWorkers(t, cmds, "failed")

	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int]("alpha", "records")
	if recs, err := mustGet(t, s, "alpha", "w0-op0"); err != nil || recs == nil {
		t.Fatalf("spot check: %v %v", recs, err)
	}
	total := 0
	if err := s.View(t.Context(), []string{"alpha"}, func(r meta.Reader) error {
		return c.Scan(t.Context(), r, func(string, *map[string]int) error {
			total++
			return nil
		})
	}); err != nil {
		t.Fatalf("reopen after storm: %v", err)
	}
	if want := workers * mpOps; total != want {
		t.Fatalf("lost writes: %d records, want %d", total, want)
	}
}

func TestMultiProcessWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	worker := os.Getenv("META_MP_WORKER")
	ops, err := strconv.Atoi(os.Getenv("META_MP_OPS"))
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int]("alpha", "records")
	ctx := t.Context()
	for i := range ops {
		id := fmt.Sprintf("w%s-op%d", worker, i)
		v := map[string]int{"n": i}
		if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
			return c.Insert(ctx, w, id, &v)
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
	cmds := contracttest.SpawnWorkers(t, 2*inverseWorkers, "TestInverseScopeWorker", func(w int) []string {
		return []string{"META_MP_DIR=" + dir, "META_MP_WORKER=" + strconv.Itoa(w), "META_MP_SCOPE=" + scopes[w%2]}
	})
	contracttest.WaitWorkers(t, cmds, "failed (deadlock or error)")
	s := newStore(t, dir, "alpha", "beta")
	for _, ns := range []string{"alpha", "beta"} {
		total := 0
		c := meta.NewCollection[map[string]int](ns, "records")
		if err := s.View(t.Context(), []string{ns}, func(r meta.Reader) error {
			return c.Scan(t.Context(), r, func(string, *map[string]int) error {
				total++
				return nil
			})
		}); err != nil {
			t.Fatalf("reopen %s: %v", ns, err)
		}
		if want := inverseWorkers * inverseOps; total != want {
			t.Fatalf("%s lost writes: %d records, want %d", ns, total, want)
		}
	}
}

func TestInverseScopeWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	worker := os.Getenv("META_MP_WORKER")
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
			v := map[string]int{"peer": seen}
			return c.Insert(ctx, w, id, &v)
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
	cmd := exec.Command(os.Args[0], "-test.run=TestAckWorker$", "-test.count=1") //nolint:gosec
	cmd.Env = append(os.Environ(), "META_MP_DIR="+dir)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var acked []string
	sc := bufio.NewScanner(pipe)
	for sc.Scan() {
		line := sc.Text()
		if id, ok := strings.CutPrefix(line, "ACK "); ok {
			acked = append(acked, id)
			if len(acked) == 25 {
				break
			}
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill worker: %v", err)
	}
	_ = cmd.Wait()
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
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	s := newStore(t, dir, "alpha")
	ctx := t.Context()
	c := meta.NewCollection[map[string]int]("alpha", "records")
	for i := range 1000 {
		id := fmt.Sprintf("op%d", i)
		v := map[string]int{"n": i}
		if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
			return c.Insert(ctx, w, id, &v)
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		fmt.Printf("ACK %s\n", id)
	}
}

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
	select {
	case <-ch:
	case <-time.After(8 * time.Second):
		t.Fatal("no event for external-process commit")
	}
}

func TestEventsWriterWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	s := newStore(t, dir, "alpha")
	ctx := t.Context()
	c := meta.NewCollection[map[string]int]("alpha", "records")
	v := map[string]int{"ext": 1}
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "ext", &v)
	}); err != nil {
		t.Fatal(err)
	}
}

func stormWorkers() int {
	if v, err := strconv.Atoi(os.Getenv("COCOON_STORM_WORKERS")); err == nil && v > 0 {
		return v
	}
	return 256
}
