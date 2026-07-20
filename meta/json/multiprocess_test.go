package json

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
)

const (
	mpWorkers = 256
	mpOps     = 15
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
	recs, err := c.Get1(t.Context(), "w0-op0")
	if err != nil || recs == nil {
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
