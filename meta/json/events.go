package json

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	eventsDebounce   = 50 * time.Millisecond
	eventsSafetyPoll = 5 * time.Second
)

// Events subscribes to committed-change signals: fsnotify on the
// parent-directory set of every namespace file, confirmed against each
// file's identity tuple (inode, size, mtime) — the thing writers rotate
// atomically — with a low-frequency safety poll as the floor for lost
// inotify events (design §7).
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
	ch, release := s.events.subscribe()
	stop := context.AfterFunc(ctx, release)
	return ch, func() { stop(); release() }, nil
}

type notifier struct {
	watcher *fsnotify.Watcher
	files   []string
	done    chan struct{}

	mu     sync.Mutex
	subs   map[int]chan struct{}
	nextID int
	tokens map[string]fileToken
}

func newNotifier(nss map[string]*nsState) (*notifier, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	n := &notifier{
		watcher: watcher,
		done:    make(chan struct{}),
		subs:    map[int]chan struct{}{},
		tokens:  map[string]fileToken{},
	}
	dirs := map[string]struct{}{}
	for _, st := range nss {
		n.files = append(n.files, st.def.FilePath)
		dirs[filepath.Dir(st.def.FilePath)] = struct{}{}
	}
	for dir := range dirs {
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return nil, err
		}
	}
	n.refreshTokens()
	go n.loop()
	return n, nil
}

func (n *notifier) subscribe() (chan struct{}, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := n.nextID
	n.nextID++
	ch := make(chan struct{}, 1)
	n.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			delete(n.subs, id)
		})
	}
}

func (n *notifier) stop() {
	close(n.done)
	_ = n.watcher.Close()
}

func (n *notifier) loop() {
	watchErrs := testWatchErrs // seam snapshot at spawn; nil outside tests
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false
	poll := time.NewTicker(eventsSafetyPoll)
	defer poll.Stop()

	for {
		select {
		case <-n.done:
			return
		case _, ok := <-n.watcher.Events:
			if !ok {
				return
			}
			if !pending {
				timer.Reset(eventsDebounce)
				pending = true
			}
		case _, ok := <-n.watcher.Errors:
			// Overflow or watch error: re-read the token immediately and signal if it moved.
			if !ok {
				return
			}
			n.checkAndBroadcast()
		case <-watchErrs: // nil outside tests: never selected
			n.checkAndBroadcast()
		case <-timer.C:
			pending = false
			n.checkAndBroadcast()
		case <-poll.C:
			n.checkAndBroadcast()
		}
	}
}

func (n *notifier) checkAndBroadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	changed := false
	for _, f := range n.files {
		tok := statToken(f)
		if n.tokens[f] != tok {
			n.tokens[f] = tok
			changed = true
		}
	}
	if !changed {
		return
	}
	for _, ch := range n.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *notifier) refreshTokens() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, f := range n.files {
		n.tokens[f] = statToken(f)
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
