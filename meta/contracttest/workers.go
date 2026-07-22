package contracttest

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// SpawnWorkers re-execs the test binary n times against the helper test testName; env yields each worker's extra environment.
func SpawnWorkers(t *testing.T, n int, testName string, env func(w int) []string) []*exec.Cmd {
	t.Helper()
	cmds := make([]*exec.Cmd, 0, n)
	for w := range n {
		cmd := exec.Command(os.Args[0], "-test.run="+testName+"$", "-test.count=1") //nolint:gosec
		cmd.Env = append(os.Environ(), env(w)...)
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
