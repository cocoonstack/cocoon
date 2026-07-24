//go:build linux

package daemon

import (
	"os/exec"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/utils"
)

const exitWaitBudget = 5 * time.Second

func TestWatcherReportsProcessExit(t *testing.T) {
	cmd, proc := startVictim(t)
	w := startWatcher(t)
	key := watchKey{backend: "fake-hv", vmID: "vm1"}

	w.ensure(key, proc, 5)
	if got := w.pidOf(key); got != proc.PID {
		t.Fatalf("got watched pid %d, want %d", got, proc.PID)
	}

	_ = cmd.Process.Kill()
	select {
	case ev := <-w.exits:
		if ev.key != key {
			t.Errorf("got key %+v, want %+v", ev.key, key)
		}
		if ev.gen != 5 {
			t.Errorf("got generation %d, want the one adopted (5)", ev.gen)
		}
	case <-time.After(exitWaitBudget):
		t.Fatal("no exit event within the budget")
	}
	if got := w.pidOf(key); got != 0 {
		t.Errorf("got watched pid %d after the exit, want the entry evicted", got)
	}
}

func TestWatcherReplacesOnNewGeneration(t *testing.T) {
	_, first := startVictim(t)
	cmd2, second := startVictim(t)
	w := startWatcher(t)
	key := watchKey{backend: "fake-hv", vmID: "vm1"}

	w.ensure(key, first, 1)
	w.ensure(key, second, 2)
	if got := w.pidOf(key); got != second.PID {
		t.Fatalf("got watched pid %d, want the newer generation %d", got, second.PID)
	}

	// Only the adopted generation may report: the replaced one is no longer watched.
	_ = cmd2.Process.Kill()
	select {
	case ev := <-w.exits:
		if ev.gen != 2 {
			t.Errorf("got generation %d, want 2", ev.gen)
		}
	case <-time.After(exitWaitBudget):
		t.Fatal("no exit event within the budget")
	}
}

func TestWatcherDropAbsentEvictsUnlistedVM(t *testing.T) {
	_, proc := startVictim(t)
	w := startWatcher(t)
	key := watchKey{backend: "fake-hv", vmID: "vm1"}

	w.ensure(key, proc, 1)
	w.dropAbsent("fake-hv", map[string]struct{}{"vm2": {}})

	if got := w.pidOf(key); got != 0 {
		t.Errorf("got watched pid %d, want the deleted VM's watch released", got)
	}
}

func TestWatcherKeepsOtherBackendsOnDropAbsent(t *testing.T) {
	_, proc := startVictim(t)
	w := startWatcher(t)
	key := watchKey{backend: "fake-hv", vmID: "vm1"}

	w.ensure(key, proc, 1)
	w.dropAbsent("other-hv", nil)

	if got := w.pidOf(key); got != proc.PID {
		t.Errorf("got watched pid %d, want another backend's scan to leave this watch alone", got)
	}
}

// startVictim launches a process the test can kill, returning its generation.
func startVictim(t *testing.T) (*exec.Cmd, utils.ProcRef) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a victim process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	proc, err := utils.ProcRefOf(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("ProcRefOf: %v", err)
	}
	return cmd, proc
}

func startWatcher(t *testing.T) *procWatcher {
	t.Helper()
	w := newProcWatcher()
	if err := w.start(t.Context()); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	t.Cleanup(w.close)
	return w
}
