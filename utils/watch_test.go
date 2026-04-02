package utils

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := WatchFile(t.Context(), target, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	// Write via atomic rename (same pattern as AtomicWriteFile).
	if err := AtomicWriteFile(target, []byte(`{"v":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch signal after atomic write")
	}
}

func TestWatchFileDebounce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := WatchFile(t.Context(), target, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	// Rapid successive writes should coalesce into one signal.
	for i := range 5 {
		if err := AtomicWriteFile(target, []byte{byte('0' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for debounced signal")
	}

	// No second signal should arrive within the debounce window.
	select {
	case <-ch:
		t.Fatal("unexpected second signal within debounce window")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatchFileCancel(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := WatchFile(ctx, target, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	// Channel should be closed after ctx cancel.
	select {
	case _, ok := <-ch:
		if ok {
			// Received a lingering signal before close — drain and wait for close.
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("channel not closed after context cancel")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for channel close")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}
