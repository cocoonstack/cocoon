package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	eventsDebounce   = 50 * time.Millisecond
	eventsSafetyPoll = 5 * time.Second
)

// Events subscribes to committed-change signals: fsnotify on the DB's parent
// dir confirmed via data_version on a pinned connection — the counter is
// only comparable across calls on ONE connection and never moves for that
// connection's own commits (§7).
func (s *Store) Events(ctx context.Context) (<-chan struct{}, func(), error) {
	if s.notifier == nil {
		n, err := newNotifier(s.path)
		if err != nil {
			return nil, nil, err
		}
		s.notifier = n
	}
	ch, release := s.notifier.subscribe()
	stop := context.AfterFunc(ctx, release)
	return ch, func() { stop(); release() }, nil
}

type notifier struct {
	watcher *fsnotify.Watcher
	pinned  *sql.Conn
	pinDB   *sql.DB
	done    chan struct{}

	mu      sync.Mutex
	subs    map[int]chan struct{}
	nextID  int
	version int64
}

func newNotifier(dbPath string) (*notifier, error) {
	db, err := open(dbPath, "FULL", false)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, mapErr(err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, err
	}
	if err := watcher.Add(filepath.Dir(dbPath)); err != nil {
		_ = watcher.Close()
		_ = conn.Close()
		_ = db.Close()
		return nil, err
	}
	n := &notifier{watcher: watcher, pinned: conn, pinDB: db, done: make(chan struct{}), subs: map[int]chan struct{}{}}
	n.version, _ = n.dataVersion()
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
	_ = n.pinned.Close()
	_ = n.pinDB.Close()
}

func (n *notifier) loop() {
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
			if !ok {
				return
			}
			n.checkAndBroadcast()
		case <-timer.C:
			pending = false
			n.checkAndBroadcast()
		case <-poll.C:
			n.checkAndBroadcast()
		}
	}
}

func (n *notifier) dataVersion() (int64, error) {
	var v int64
	err := n.pinned.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&v)
	return v, err
}

func (n *notifier) checkAndBroadcast() {
	v, err := n.dataVersion()
	if err != nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if v == n.version {
		return
	}
	n.version = v
	for _, ch := range n.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
