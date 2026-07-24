package utils

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestWriteReadPIDFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	if err := WritePIDFile(path, 12345); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	got, err := ReadPIDFile(path)
	if err != nil {
		t.Fatalf("ReadPIDFile: %v", err)
	}
	if got != 12345 {
		t.Errorf("PID: got %d, want 12345", got)
	}
}

func TestReadPIDFile_NotExist(t *testing.T) {
	_, err := ReadPIDFile("/nonexistent/pid")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestReadPIDFile_InvalidContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pid")
	if err := os.WriteFile(path, []byte("not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadPIDFile(path)
	if err == nil {
		t.Fatal("expected error for invalid content")
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

func TestWriteReadPIDFile_Roundtrip_LargePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.pid")

	// Max PID on Linux is typically 4194304 (2^22).
	const largePID = 4194304
	if err := WritePIDFile(path, largePID); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPIDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != largePID {
		t.Errorf("PID: got %d, want %d", got, largePID)
	}
}

func TestReadPIDFile_WhitespaceHandling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ws.pid")
	if err := os.WriteFile(path, []byte("  42  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPIDFile(path)
	if err != nil {
		t.Fatalf("ReadPIDFile: %v", err)
	}
	if got != 42 {
		t.Errorf("PID: got %d, want 42", got)
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
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	if IsProcessAlive(pid) {
		t.Skip("PID recycled too quickly, skipping")
	}
}

func TestVerifyProcessCmdline_InvalidPID(t *testing.T) {
	if VerifyProcessCmdline(0, "x", "y") {
		t.Error("expected false for PID 0")
	}
	if VerifyProcessCmdline(-1, "x", "y") {
		t.Error("expected false for PID -1")
	}
}

func TestVerifyProcessCmdline_WrongBinary(t *testing.T) {
	pid := os.Getpid()
	result := VerifyProcessCmdline(pid, "definitely-not-the-binary", "definitely-not-the-arg")
	_ = result // Just verify no panic.
}

func TestTerminateProcess_SleepProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	waitForExec(t, pid, "sleep", "60")

	// Reap the child in background so it doesn't become a zombie after SIGTERM.
	// Without this, kill(pid, 0) keeps returning nil for zombies and WaitFor times out.
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-waitDone
	}()

	if !IsProcessAlive(pid) {
		t.Fatal("sleep process should be alive")
	}

	ctx := t.Context()
	err := TerminateProcess(ctx, pid, "sleep", "60", 2*time.Second)
	if err != nil {
		t.Fatalf("TerminateProcess: %v", err)
	}

	<-waitDone
}

func TestTerminateProcess_AlreadyDead(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	ctx := t.Context()
	// Should return nil — process doesn't exist, VerifyProcessCmdline returns false.
	err := TerminateProcess(ctx, pid, "true", "true", 1*time.Second)
	if err != nil {
		t.Fatalf("TerminateProcess on dead process: %v", err)
	}
}

func TestTerminateProcess_InvalidPID(t *testing.T) {
	ctx := t.Context()
	// PID 0 → VerifyProcessCmdline returns false → return nil immediately.
	if err := TerminateProcess(ctx, 0, "x", "x", time.Second); err != nil {
		t.Errorf("expected nil for PID 0, got %v", err)
	}
	if err := TerminateProcess(ctx, -1, "x", "x", time.Second); err != nil {
		t.Errorf("expected nil for PID -1, got %v", err)
	}
}

func TestTerminateProcess_SIGTERMIgnored_FallsBackToKill(t *testing.T) {
	// Traps SIGTERM so only SIGKILL can end it. The trailing "; :" stops bash from
	// tail-exec'ing into sleep, which would rename argv0 and fail the "bash" cmdline check.
	cmd := exec.Command("bash", "-c", `trap "" TERM; sleep 60; :`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	waitForExec(t, pid, "bash", "sleep")

	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-waitDone
	}()

	// Very short grace period — SIGTERM won't kill it, fallback to SIGKILL.
	ctx := t.Context()
	err := TerminateProcess(ctx, pid, "bash", "sleep", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("TerminateProcess: %v", err)
	}

	<-waitDone
}

func TestFindVMMByCmdline(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("FindVMMByCmdline scans /proc — linux only")
	}
	marker := "cocoon-find-marker-" + strconv.Itoa(os.Getpid())
	// "sleep 60 && :" is a compound command so sh can't tail-exec into sleep and lose the marker arg.
	cmd := exec.Command("sh", "-c", "sleep 60 && :", marker)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Poll briefly: cmdline is written by execve, so the parent may scan before /proc/<pid>/cmdline reflects argv.
	var pids []int
	for range 50 {
		got, err := FindVMMByCmdline("sh", marker)
		if err != nil {
			t.Fatalf("FindVMMByCmdline: %v", err)
		}
		if len(got) > 0 {
			pids = got
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(pids) != 1 || pids[0] != cmd.Process.Pid {
		t.Errorf("FindVMMByCmdline: got %v, want [%d]", pids, cmd.Process.Pid)
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
	waitForExec(t, pid, "sleep", "60")
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Cancelled ctx: may return WaitFor's ctx error but must still attempt the kill.
	_ = TerminateProcess(ctx, pid, "sleep", "60", 100*time.Millisecond)
}

// waitForExec parks until the child's execve lands: cmd.Start returns
// pre-exec, and TerminateProcess declines a mismatched cmdline (#87 race).
func waitForExec(t *testing.T, pid int, binaryName, expectArg string) {
	t.Helper()
	if err := WaitFor(t.Context(), 5*time.Second, time.Millisecond, func() (bool, error) {
		return VerifyProcessCmdline(pid, binaryName, expectArg), nil
	}); err != nil {
		t.Fatalf("child never execed into %s: %v", binaryName, err)
	}
}
