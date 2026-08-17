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

// Broadcaster is the engine-shared half of an Events notifier: it owns the subscriber set and the debounce/poll loop, funneling every trigger into the engine's check func — which calls Broadcast only when state moved.
type Broadcaster struct {
	watcher *fsnotify.Watcher
	done    chan struct{}

	mu     sync.Mutex
	subs   map[int]chan struct{}
	nextID int
}

// NewBroadcaster wraps an fsnotify watcher the engine has already populated.
func NewBroadcaster(watcher *fsnotify.Watcher) *Broadcaster {
	return &Broadcaster{watcher: watcher, done: make(chan struct{}), subs: map[int]chan struct{}{}}
}

// Subscribe registers a coalescing signal channel; release is idempotent.
func (b *Broadcaster) Subscribe() (chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan struct{}, 1)
	b.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.subs, id)
		})
	}
}

// Broadcast signals every subscriber without blocking.
func (b *Broadcaster) Broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
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

// Run is the notifier goroutine body: watcher events are debounced; watcher errors, the optional extra trigger and a safety poll check immediately.
func (b *Broadcaster) Run(check func(), extra <-chan struct{}) {
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false
	poll := time.NewTicker(eventsSafetyPoll)
	defer poll.Stop()
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
		case <-extra:
			check()
		case <-timer.C:
			pending = false
			check()
		case <-poll.C:
			check()
		}
	}
}
