package contracttest

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta"
)

// SpawnWorkers re-execs the test binary n times against the helper test testName in dir; env yields each worker's extra environment.
func SpawnWorkers(t *testing.T, n int, testName, dir string, env func(w int) []string) []*exec.Cmd {
	t.Helper()
	cmds := make([]*exec.Cmd, 0, n)
	for w := range n {
		cmd := WorkerCmd(testName, "META_MP_DIR="+dir, "META_MP_WORKER="+strconv.Itoa(w))
		cmd.Env = append(cmd.Env, env(w)...)
		out := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker %d: %v", w, err)
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}

// WaitWorkers fails on the first worker that exits non-zero, echoing its combined output; what labels the failure.
func WaitWorkers(t *testing.T, cmds []*exec.Cmd, what string) {
	t.Helper()
	for w, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("worker %d %s: %v\n%s", w, what, err, cmd.Stdout)
		}
	}
}

func WorkerCmd(testName string, env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run="+testName+"$", "-test.count=1") //nolint:gosec
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

func HelperEnv(t *testing.T) (dir, worker string) {
	t.Helper()
	dir = os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	return dir, os.Getenv("META_MP_WORKER")
}

func StormWorkers() int {
	if v, err := strconv.Atoi(os.Getenv("COCOON_STORM_WORKERS")); err == nil && v > 0 {
		return v
	}
	return 256
}

func CollectAcks(t *testing.T, cmd *exec.Cmd, n int) []string {
	t.Helper()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var acked []string
	sc := bufio.NewScanner(pipe)
	for len(acked) < n && sc.Scan() {
		if id, ok := strings.CutPrefix(sc.Text(), "ACK "); ok {
			acked = append(acked, id)
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill worker: %v", err)
	}
	_ = cmd.Wait()
	return acked
}

func CountRaw(t *testing.T, s meta.Store, ns, table string) int {
	t.Helper()
	n := 0
	if err := s.View(t.Context(), []string{ns}, func(r meta.Reader) error {
		return r.ScanRaw(t.Context(), ns, table, func(string, jsontext.Value) error {
			n++
			return nil
		})
	}); err != nil {
		t.Fatalf("reopen %s: %v", ns, err)
	}
	return n
}

func WaitEvent(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("no event within %s (%s)", within, what)
	}
}

func ExternalProcessEvents(t *testing.T, open func(t *testing.T, dir string) meta.Store, worker string) {
	t.Helper()
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	ch, release, err := open(t, dir).Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := WorkerCmd(worker, "META_MP_DIR="+dir).Run(); err != nil {
		t.Fatalf("external writer: %v", err)
	}
	WaitEvent(t, ch, 8*time.Second, "external-process commit")
}

func HoldWriter(t *testing.T, s meta.Store, sc meta.Scope, after meta.UpdateFunc) (release func()) {
	t.Helper()
	held := make(chan struct{})
	unblock := make(chan struct{})
	holderErr := make(chan error, 1)
	markHeld := sync.OnceFunc(func() { close(held) })
	go func() {
		holderErr <- s.Update(t.Context(), sc, meta.CommitDurable, func(w meta.Writer) error {
			markHeld()
			<-unblock
			return after(w)
		})
	}()
	select {
	case <-held:
	case err := <-holderErr:
		t.Fatalf("holder update ended before holding the writer: %v", err)
	}
	return func() {
		close(unblock)
		if err := <-holderErr; err != nil {
			t.Errorf("holder update: %v", err)
		}
	}
}
