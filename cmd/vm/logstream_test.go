package vm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/utils"
)

func TestWatchFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := watchFile(t.Context(), target, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	if err := utils.AtomicWriteFile(target, []byte(`{"v":1}`), 0o644, utils.Sync); err != nil {
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

	const debounce = 200 * time.Millisecond
	ch, err := watchFile(t.Context(), target, debounce)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for i := range 5 {
		if err := utils.AtomicWriteFile(target, []byte{byte('0' + i)}, 0o644, utils.NoSync); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for debounced signal")
	}
	if elapsed := time.Since(start); elapsed < debounce {
		t.Fatalf("signal after %s, want it held for the %s debounce", elapsed, debounce)
	}

	select {
	case <-ch:
		t.Fatal("unexpected second signal within debounce window")
	case <-time.After(debounce / 2):
	}
}

func TestWatchFileCancel(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	ch, err := watchFile(ctx, target, 50*time.Millisecond)
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

func TestSeekToLastNLines(t *testing.T) {
	tests := []struct {
		name string
		body string
		n    int
		want string
	}{
		{name: "no trailing newline", body: "a\nb\nc", n: 2, want: "b\nc"},
		{name: "trailing newline", body: "a\nb\nc\n", n: 2, want: "b\nc\n"},
		{name: "more than available", body: "a\nb", n: 5, want: "a\nb"},
		{name: "one line trailing", body: "a\nb\nc\n", n: 1, want: "c\n"},
		{name: "empty", body: "", n: 3, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "log")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(tt.body); err != nil {
				t.Fatal(err)
			}
			if err := seekToLastNLines(f, tt.n); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
