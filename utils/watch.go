package utils

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchFile watches a file via fsnotify on the parent dir (atomic rename changes inode).
func WatchFile(ctx context.Context, filePath string, debounce time.Duration) (<-chan struct{}, error) {
	return watch(ctx, filepath.Dir(filePath), filepath.Base(filePath), debounce)
}

// WatchDir watches every entry of dir — for stores keeping one file per item, where any entry create/write/rename/remove means the set changed.
func WatchDir(ctx context.Context, dir string, debounce time.Duration) (<-chan struct{}, error) {
	return watch(ctx, dir, "", debounce)
}

func watch(ctx context.Context, dir, base string, debounce time.Duration) (<-chan struct{}, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
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
			// base=="" watches every entry (WatchDir); Remove matters there — deleting an item's file changes the set.
			if base != "" && filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
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
