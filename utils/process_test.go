package utils

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestWriteReadPIDFile(t *testing.T) {
	tests := []struct {
		name string
		pid  int
	}{
		{"typical", 12345},
		{"linux max pid", 4194304},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.pid")
			if err := WritePIDFile(path, tt.pid); err != nil {
				t.Fatalf("WritePIDFile: %v", err)
			}
			got, err := ReadPIDFile(path)
			if err != nil {
				t.Fatalf("ReadPIDFile: %v", err)
			}
			if got != tt.pid {
				t.Errorf("PID: got %d, want %d", got, tt.pid)
			}
		})
	}
}

func TestReadPIDFile_NotExist(t *testing.T) {
	_, err := ReadPIDFile("/nonexistent/pid")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestReadPIDFile_Content(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
		wantErr bool
	}{
		{"invalid content", "not-a-number\n", 0, true},
		{"whitespace handling", "  42  \n", 42, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "content.pid")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadPIDFile(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ReadPIDFile err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("PID: got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWritePIDFile_Permissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.pid")
	if err := WritePIDFile(path, 1); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("permissions: got %o, want 0600", fi.Mode().Perm())
	}
}

func TestIsProcessAlive_Self(t *testing.T) {
	if !IsProcessAlive(os.Getpid()) {
		t.Error("expected current process to be alive")
	}
}

func TestIsProcessAlive_InvalidPID(t *testing.T) {
	tests := []struct {
		name string
		pid  int
	}{
		{"zero", 0},
		{"negative", -1},
		{"large_negative", -99999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if IsProcessAlive(tt.pid) {
				t.Errorf("IsProcessAlive(%d) = true, want false", tt.pid)
			}
		})
	}
}

func TestIsProcessAlive_DeadProcess(t *testing.T) {
	if pid := deadPID(t); IsProcessAlive(pid) {
		t.Errorf("reaped pid %d reported alive", pid)
	}
}

func TestVerifyProcessCmdline_InvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if VerifyProcessCmdline(pid, "y") {
			t.Errorf("expected false for PID %d", pid)
		}
	}
}

func TestTerminateProcess_SleepProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	waitForExec(t, pid, "60")

	waitDone := reapChild(t, cmd)

	if !IsProcessAlive(pid) {
		t.Fatal("sleep process should be alive")
	}

	err := TerminateProcess(t.Context(), pid, "60", 2*time.Second)
	if err != nil {
		t.Fatalf("TerminateProcess: %v", err)
	}

	<-waitDone
}

func TestTerminateProcess_AlreadyDead(t *testing.T) {
	err := TerminateProcess(t.Context(), deadPID(t), "true", time.Second)
	if err != nil {
		t.Fatalf("TerminateProcess on dead process: %v", err)
	}
}

func TestTerminateProcess_InvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if err := TerminateProcess(t.Context(), pid, "x", time.Second); err != nil {
			t.Errorf("expected nil for PID %d, got %v", pid, err)
		}
	}
}

func TestTerminateProcess_SIGTERMIgnored_FallsBackToKill(t *testing.T) {
	const grace = 200 * time.Millisecond
	cmd := exec.Command("bash", "-c", `trap "" TERM; echo ready; sleep 60; :`, "vmm-sock")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	if line, readErr := bufio.NewReader(stdout).ReadString('\n'); readErr != nil || line != "ready\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("child did not install its TERM trap: %q, %v", line, readErr)
	}
	waitForExec(t, pid, "vmm-sock")

	waitDone := reapChild(t, cmd)

	start := time.Now()
	if err := TerminateProcess(t.Context(), pid, "vmm-sock", grace); err != nil {
		t.Fatalf("TerminateProcess: %v", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("returned after %s, before the %s grace: SIGTERM was not ignored or SIGKILL came early", elapsed, grace)
	}

	<-waitDone
}

func TestFindVMMByCmdline(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("FindVMMByCmdline scans /proc — linux only")
	}
	marker := "cocoon-find-marker-" + strconv.Itoa(os.Getpid())

	cmd := exec.Command("sh", "-c", "sleep 60 && :", marker)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	reapChild(t, cmd)

	var pids []int
	if err := WaitFor(t.Context(), 500*time.Millisecond, 10*time.Millisecond, func() (bool, error) {
		got, err := FindVMMByCmdline("sh", marker)
		pids = got
		return len(got) > 0, err
	}); err != nil {
		t.Fatalf("FindVMMByCmdline: %v", err)
	}
	if !slices.Contains(pids, cmd.Process.Pid) {
		t.Errorf("FindVMMByCmdline never matched the execed cmdline: got %v, want %d", pids, cmd.Process.Pid)
	}

	if got, _ := FindVMMByCmdline("definitely-no-such-binary", marker); len(got) != 0 {
		t.Errorf("wrong-binary scan matched: %v", got)
	}
	if got, _ := FindVMMByCmdline("sh", "no-such-marker-"+marker); len(got) != 0 {
		t.Errorf("wrong-marker scan matched: %v", got)
	}
}

func TestTerminateProcess_ContextCancelled(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	waitForExec(t, pid, "60")

	waitDone := reapChild(t, cmd)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := TerminateProcess(ctx, pid, "60", 100*time.Millisecond); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("TerminateProcess = %v, want nil or context.Canceled", err)
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled context left the process alive")
	}
}

func TestWaitProcessExitReturnsOnExit(t *testing.T) {
	cmd := exec.Command("sleep", "0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()

	const timeout = 5 * time.Second
	start := time.Now()
	if err := WaitProcessExit(t.Context(), pid, timeout); err != nil {
		t.Fatalf("WaitProcessExit: %v", err)
	}
	if elapsed := time.Since(start); elapsed > timeout/5 {
		t.Fatalf("elapsed = %s, want the wait to end with the process, not to poll out %s", elapsed, timeout)
	}
}

func TestWaitProcessExitTimesOutOnALiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	reapChild(t, cmd)

	err := WaitProcessExit(t.Context(), cmd.Process.Pid, 50*time.Millisecond)
	if err == nil {
		t.Fatal("WaitProcessExit = nil, want a timeout while the process is alive")
	}
}

func reapChild(t *testing.T, cmd *exec.Cmd) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return done
}

func waitForExec(t *testing.T, pid int, expectArg string) {
	t.Helper()
	if err := WaitFor(t.Context(), 5*time.Second, time.Millisecond, func() (bool, error) {
		return VerifyProcessCmdline(pid, expectArg), nil
	}); err != nil {
		t.Fatalf("child never execed into %s, so TerminateProcess would decline the mismatched cmdline (#87): %v", expectArg, err)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	return cmd.Process.Pid
}
