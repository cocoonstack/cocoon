package meta

import (
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	eventsDebounce   = 50 * time.Millisecond
	eventsSafetyPoll = 5 * time.Second
)

// Broadcaster is the engine-shared half of an Events notifier: the subscriber set and the debounce/poll loop feeding the engine's check func.
type Broadcaster struct {
	watcher *fsnotify.Watcher
	done    chan struct{}

	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// NewBroadcaster wraps an fsnotify watcher the engine has already populated.
func NewBroadcaster(watcher *fsnotify.Watcher) *Broadcaster {
	return &Broadcaster{watcher: watcher, done: make(chan struct{}), subs: map[chan struct{}]struct{}{}}
}

// Subscribe registers a coalescing signal channel; release is idempotent.
func (b *Broadcaster) Subscribe() (chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{}, 1)
	b.subs[ch] = struct{}{}
	return ch, sync.OnceFunc(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs, ch)
	})
}

// Broadcast signals every subscriber without blocking.
func (b *Broadcaster) Broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Stop ends Run and closes the watcher.
func (b *Broadcaster) Stop() {
	close(b.done)
	_ = b.watcher.Close()
}

// Run is the notifier goroutine body.
func (b *Broadcaster) Run(check func()) {
	timer := time.NewTimer(0)
	timer.Stop()
	pending := false
	poll := time.Tick(eventsSafetyPoll)
	for {
		select {
		case <-b.done:
			return
		case _, ok := <-b.watcher.Events:
			if !ok {
				return
			}
			if !pending {
				timer.Reset(eventsDebounce)
				pending = true
			}
		case _, ok := <-b.watcher.Errors:
			if !ok {
				return
			}
			check()
		case <-timer.C:
			pending = false
			check()
		case <-poll:
			check()
		}
	}
}
