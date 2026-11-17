package vmlock

import (
	"os/exec"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/lock/flock"
)

func TestSharedLeaseInheritanceSurvivesParentDescriptorClose(t *testing.T) {
	rootDir := t.TempDir()
	lease, err := NewSharedLease(t.Context(), rootDir, "source")
	if err != nil {
		t.Fatalf("new shared lease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = append(cmd.ExtraFiles, lease.File())
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lease holder: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if err := lease.Close(); err != nil {
		t.Fatalf("close parent descriptor: %v", err)
	}

	assertWriterWaitsFor(t, rootDir, func() {
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("kill lease holder: %v", err)
		}
		_ = cmd.Wait()
	})
}

func TestSharedLeaseInheritanceSurvivesChildExit(t *testing.T) {
	rootDir := t.TempDir()
	lease, err := NewSharedLease(t.Context(), rootDir, "source")
	if err != nil {
		t.Fatalf("new shared lease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	cmd := exec.Command("true")
	cmd.ExtraFiles = append(cmd.ExtraFiles, lease.File())
	if err := cmd.Run(); err != nil {
		t.Fatalf("run lease holder: %v", err)
	}

	assertWriterWaitsFor(t, rootDir, func() {
		if err := lease.Close(); err != nil {
			t.Fatalf("close parent lease: %v", err)
		}
	})
}

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

	leaseCh := make(chan *flock.SharedLease, 1)
	errCh := make(chan error, 1)
	go func() {
		lease, err := NewSharedLease(ctx, rootDir, "source")
		errCh <- err
		leaseCh <- lease
	}()

	select {
	case err := <-errCh:
		t.Fatalf("lease acquired despite the held exclusive: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := ex.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("lease after exclusive unlink: %v", err)
	}
	lease := <-leaseCh
	defer lease.Close()

	probe, err := New(rootDir, "source")
	if err != nil {
		t.Fatal(err)
	}
	if got, tryErr := probe.TryLock(ctx); tryErr != nil || got {
		t.Fatalf("exclusive acquired despite a live lease (split lock): got=%v err=%v", got, tryErr)
	}
}

func assertWriterWaitsFor(t *testing.T, rootDir string, release func()) {
	t.Helper()
	writer, err := New(rootDir, "source")
	if err != nil {
		t.Fatalf("new writer lock: %v", err)
	}
	if got, tryErr := writer.TryLock(t.Context()); tryErr != nil || got {
		t.Fatalf("writer while lease held: got=%v err=%v", got, tryErr)
	}
	release()
	if got, tryErr := writer.TryLock(t.Context()); tryErr != nil || !got {
		t.Fatalf("writer after lease released: got=%v err=%v", got, tryErr)
	}
	if err := writer.Unlock(t.Context()); err != nil {
		t.Fatalf("unlock writer: %v", err)
	}
}
