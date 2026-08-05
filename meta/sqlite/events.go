package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/cocoonstack/cocoon/meta"
)

// Events subscribes to committed-change signals: fsnotify on the DB's parent dir confirmed via data_version on a pinned connection — the counter is only comparable across calls on ONE connection and never moves for that connection's own commits (§7).
func (s *Store) Events(ctx context.Context) (<-chan struct{}, func(), error) {
	s.mu.Lock()
	if s.notifier == nil {
		n, err := newNotifier(s.path)
		if err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
		s.notifier = n
	}
	n := s.notifier
	s.mu.Unlock()
	ch, release := n.b.Subscribe()
	stop := context.AfterFunc(ctx, release)
	return ch, func() { stop(); release() }, nil
}

// notifier holds the pinned data_version connection; version is touched only by the init call and the Run goroutine. ctx is the notifier's own lifetime — it outlives every Events caller and ends at stop.
type notifier struct {
	b       *meta.Broadcaster
	watcher *fsnotify.Watcher // kept for the severed-watch test seam
	pinned  *sql.Conn
	pinDB   *sql.DB
	ctx     context.Context
	cancel  context.CancelFunc
	version int64
}

func newNotifier(dbPath string) (*notifier, error) {
	ctx, cancel := context.WithCancel(context.Background())
	db, err := open(dbPath, "FULL", false)
	if err != nil {
		cancel()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		cancel()
		_ = db.Close()
		return nil, mapErr(err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		cancel()
		_ = conn.Close()
		_ = db.Close()
		return nil, err
	}
	if err := watcher.Add(filepath.Dir(dbPath)); err != nil {
		cancel()
		_ = watcher.Close()
		_ = conn.Close()
		_ = db.Close()
		return nil, err
	}
	n := &notifier{b: meta.NewBroadcaster(watcher), watcher: watcher, pinned: conn, pinDB: db, ctx: ctx, cancel: cancel}
	n.version, _ = n.dataVersion()
	go n.b.Run(n.check, nil)
	return n, nil
}

func (n *notifier) stop() {
	n.b.Stop()
	n.cancel()
	_ = n.pinned.Close()
	_ = n.pinDB.Close()
}

func (n *notifier) check() {
	v, err := n.dataVersion()
	if err != nil || v == n.version {
		return
	}
	n.version = v
	n.b.Broadcast()
}

func (n *notifier) dataVersion() (int64, error) {
	var v int64
	err := n.pinned.QueryRowContext(n.ctx, "PRAGMA data_version").Scan(&v)
	return v, err
}
