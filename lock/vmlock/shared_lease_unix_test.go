//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package vmlock

import (
	"os/exec"
	"testing"
)

func TestSharedLeaseInheritanceSurvivesParentDescriptorClose(t *testing.T) {
	rootDir := t.TempDir()
	lease, err := NewSharedLease(t.Context(), rootDir, "source")
	if err != nil {
		t.Fatalf("new shared lease: %v", err)
	}

	cmd := exec.Command("sleep", "30") //nolint:gosec // fixed test helper
	cmd.ExtraFiles = append(cmd.ExtraFiles, lease.File())
	if err := cmd.Start(); err != nil {
		_ = lease.Close()
		t.Fatalf("start lease holder: %v", err)
	}
	if err := lease.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("close parent descriptor: %v", err)
	}

	writer, err := New(rootDir, "source")
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("new writer lock: %v", err)
	}
	if got, tryErr := writer.TryLock(t.Context()); tryErr != nil || got {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("writer while child holds lease: got=%v err=%v", got, tryErr)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill lease holder: %v", err)
	}
	_ = cmd.Wait()
	if got, tryErr := writer.TryLock(t.Context()); tryErr != nil || !got {
		t.Fatalf("writer after child exits: got=%v err=%v", got, tryErr)
	}
	if err := writer.Unlock(t.Context()); err != nil {
		t.Fatalf("unlock writer: %v", err)
	}
}

func TestSharedLeaseInheritanceSurvivesChildExit(t *testing.T) {
	rootDir := t.TempDir()
	lease, err := NewSharedLease(t.Context(), rootDir, "source")
	if err != nil {
		t.Fatalf("new shared lease: %v", err)
	}

	cmd := exec.Command("true") //nolint:gosec // fixed test helper
	cmd.ExtraFiles = append(cmd.ExtraFiles, lease.File())
	if err := cmd.Run(); err != nil {
		_ = lease.Close()
		t.Fatalf("run lease holder: %v", err)
	}

	writer, err := New(rootDir, "source")
	if err != nil {
		_ = lease.Close()
		t.Fatalf("new writer lock: %v", err)
	}
	if got, tryErr := writer.TryLock(t.Context()); tryErr != nil || got {
		_ = lease.Close()
		t.Fatalf("writer while parent holds lease: got=%v err=%v", got, tryErr)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("close parent lease: %v", err)
	}
	if got, tryErr := writer.TryLock(t.Context()); tryErr != nil || !got {
		t.Fatalf("writer after parent closes: got=%v err=%v", got, tryErr)
	}
	if err := writer.Unlock(t.Context()); err != nil {
		t.Fatalf("unlock writer: %v", err)
	}
}
