package json

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/fsnotify/fsnotify"

	"github.com/cocoonstack/cocoon/meta"
)

// Events subscribes to committed-change signals: fsnotify confirmed against each file's (inode, size, mtime) identity, with a safety poll as a floor for missed events.
func (s *Store) Events(ctx context.Context) (<-chan struct{}, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, os.ErrClosed
	}
	if s.events == nil {
		n, err := newNotifier(s.nss)
		if err != nil {
			return nil, nil, err
		}
		s.events = n
	}
	ch, release := s.events.b.Subscribe()
	stop := context.AfterFunc(ctx, release)
	return ch, func() { stop(); release() }, nil
}

// notifier holds the engine-specific change detector; tokens are touched
// only by the init call and the Run goroutine.
type notifier struct {
	b       *meta.Broadcaster
	watcher *fsnotify.Watcher // kept for the severed-watch test seam
	files   []string
	tokens  map[string]fileToken
}

func newNotifier(nss map[string]*nsState) (*notifier, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	n := &notifier{b: meta.NewBroadcaster(watcher), watcher: watcher, tokens: map[string]fileToken{}}
	dirs := map[string]struct{}{}
	for _, st := range nss {
		n.files = append(n.files, st.def.FilePath)
		dirs[filepath.Dir(st.def.FilePath)] = struct{}{}
	}
	for dir := range dirs {
		// A namespace whose backend has not run yet has no directory; the safety poll covers it, so a missing dir must not disable the subscription.
		if err := watcher.Add(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = watcher.Close()
			return nil, err
		}
	}
	n.check()
	go n.b.Run(n.check, testWatchErrs)
	return n, nil
}

func (n *notifier) check() {
	changed := false
	for _, f := range n.files {
		tok := statToken(f)
		if n.tokens[f] != tok {
			n.tokens[f] = tok
			changed = true
		}
	}
	if changed {
		n.b.Broadcast()
	}
}

type fileToken struct {
	ino   uint64
	size  int64
	mtime int64
}

func statToken(path string) fileToken {
	info, err := os.Stat(path)
	if err != nil {
		return fileToken{}
	}
	tok := fileToken{size: info.Size(), mtime: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		tok.ino = st.Ino
	}
	return tok
}
