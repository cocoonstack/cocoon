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

	if err := AtomicWriteFile(target, []byte(`{"v":1}`), 0o644, Sync); err != nil {
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

	for i := range 5 {
		if err := AtomicWriteFile(target, []byte{byte('0' + i)}, 0o644, Sync); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for debounced signal")
	}

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

	ctx, cancel := context.WithCancel(t.Context())
	ch, err := WatchFile(ctx, target, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
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
