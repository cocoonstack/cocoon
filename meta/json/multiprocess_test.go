package json

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
)

const (
	mpWorkers = 256
	mpOps     = 15

	inverseWorkers = 8
	inverseOps     = 10
)

// TestMultiProcessCorrectness is the design §9 gate with real processes, not
// goroutines: every worker's acknowledged insert must be present afterwards,
// every failure a mapped taxonomy error, and the reopened store uncorrupted.
func TestMultiProcessCorrectness(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	cmds := make([]*exec.Cmd, 0, mpWorkers)
	for w := range mpWorkers {
		cmd := exec.Command(os.Args[0], "-test.run=TestMultiProcessWorker$", "-test.count=1") //nolint:gosec
		cmd.Env = append(os.Environ(),
			"META_MP_DIR="+dir,
			"META_MP_WORKER="+strconv.Itoa(w),
			fmt.Sprintf("META_MP_OPS=%d", mpOps),
		)
		out := &prefixedBuf{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker %d: %v", w, err)
		}
		cmds = append(cmds, cmd)
	}
	for w, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("worker %d failed: %v\n%s", w, err, cmd.Stdout)
		}
	}

	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int](s, "alpha", "records")
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
	if want := mpWorkers * mpOps; total != want {
		t.Fatalf("lost writes: %d records, want %d", total, want)
	}
}

// TestMultiProcessWorker is the helper-process body; it only runs when
// re-invoked by TestMultiProcessCorrectness with the env set.
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
	c := meta.NewCollection[map[string]int](s, "alpha", "records")
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

type prefixedBuf struct{ b []byte }

func (p *prefixedBuf) Write(data []byte) (int, error) {
	p.b = append(p.b, data...)
	return len(data), nil
}

func (p *prefixedBuf) String() string { return string(p.b) }

// TestInverseScopeNoDeadlock is the §9 cross-process gate: mutually-inverse
// scopes (write alpha read beta vs write beta read alpha) storm concurrently
// and must never deadlock — the engine's fixed global lock order is the proof.
func TestInverseScopeNoDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	scopes := []string{"alpha:beta", "beta:alpha"}
	cmds := make([]*exec.Cmd, 0, 2*inverseWorkers)
	for w := range 2 * inverseWorkers {
		cmd := exec.Command(os.Args[0], "-test.run=TestInverseScopeWorker$", "-test.count=1") //nolint:gosec
		cmd.Env = append(os.Environ(),
			"META_MP_DIR="+dir,
			"META_MP_WORKER="+strconv.Itoa(w),
			"META_MP_SCOPE="+scopes[w%2],
		)
		out := &prefixedBuf{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker %d: %v", w, err)
		}
		cmds = append(cmds, cmd)
	}
	for w, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("worker %d failed (deadlock or error): %v\n%s", w, err, cmd.Stdout)
		}
	}
	s := newStore(t, dir, "alpha", "beta")
	for _, ns := range []string{"alpha", "beta"} {
		total := 0
		c := meta.NewCollection[map[string]int](s, ns, "records")
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

// TestInverseScopeWorker is the helper-process body for the inverse-scope gate.
func TestInverseScopeWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	worker := os.Getenv("META_MP_WORKER")
	write, read, _ := strings.Cut(os.Getenv("META_MP_SCOPE"), ":")
	s := newStore(t, dir, "alpha", "beta")
	ctx := t.Context()
	c := meta.NewCollection[map[string]int](s, write, "records")
	peer := meta.NewCollection[map[string]int](s, read, "records")
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

// TestAckedDurableSurvivesKill is the §9 durability gate: every insert the
// worker ACKed after a CommitDurable return must be present after SIGKILL.
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

// TestAckWorker is the helper-process body for the durability gate.
func TestAckWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	s := newStore(t, dir, "alpha")
	ctx := t.Context()
	c := meta.NewCollection[map[string]int](s, "alpha", "records")
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
