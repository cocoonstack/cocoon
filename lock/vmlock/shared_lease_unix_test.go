//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package vmlock

import (
	"os/exec"
	"testing"
	"time"
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

// The split regression: an exclusive transient release unlinks the inode a waiting lease acquired; the lease must rebind so a later exclusive still sees it.
func TestSharedLeaseRebindsAfterExclusiveUnlink(t *testing.T) {
	rootDir := t.TempDir()
	ctx := t.Context()
	ex, err := New(rootDir, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Lock(ctx); err != nil {
		t.Fatal(err)
	}

	leaseCh := make(chan *SharedLease, 1)
	errCh := make(chan error, 1)
	go func() {
		lease, err := NewSharedLease(ctx, rootDir, "source")
		errCh <- err
		leaseCh <- lease
	}()

	select {
	case err := <-errCh:
		t.Fatalf("lease acquired despite the held exclusive: %v", err)
	case <-time.After(20 * time.Millisecond): // the lease is blocked on the held exclusive
	}
	if err := ex.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("lease after exclusive unlink: %v", err)
	}
	lease := <-leaseCh
	defer lease.Close() //nolint:errcheck

	probe, err := New(rootDir, "source")
	if err != nil {
		t.Fatal(err)
	}
	if got, tryErr := probe.TryLock(ctx); tryErr != nil || got {
		t.Fatalf("exclusive acquired despite a live lease (split lock): got=%v err=%v", got, tryErr)
	}
}
