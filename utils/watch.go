package utils

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchFile watches a file for changes using fsnotify on the parent directory.
// It sends on the returned channel after each change, debounced by the given duration.
// The channel is closed when ctx is canceled. The caller should re-read the file
// after receiving a signal.
//
// Watching the parent directory (rather than the file itself) is required because
// AtomicWriteJSON uses temp-file + rename, which changes the file's inode.
func WatchFile(ctx context.Context, filePath string, debounce time.Duration) (<-chan struct{}, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return nil, err
	}

	ch := make(chan struct{}, 1)
	go watchLoop(ctx, watcher, base, debounce, ch)
	return ch, nil
}

func watchLoop(ctx context.Context, watcher *fsnotify.Watcher, base string, debounce time.Duration, ch chan<- struct{}) {
	defer close(ch)
	defer watcher.Close() //nolint:errcheck

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
				continue
			}
			if !pending {
				timer.Reset(debounce)
				pending = true
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		case <-timer.C:
			pending = false
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}
