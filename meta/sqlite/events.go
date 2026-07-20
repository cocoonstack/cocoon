package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/cocoonstack/cocoon/meta"
)

// Events subscribes to committed-change signals: fsnotify on the DB's parent
// dir confirmed via data_version on a pinned connection — the counter is
// only comparable across calls on ONE connection and never moves for that
// connection's own commits (§7).
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

// notifier holds the pinned data_version connection; version is touched only
// by the init call and the Run goroutine.
type notifier struct {
	b       *meta.Broadcaster
	pinned  *sql.Conn
	pinDB   *sql.DB
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
		_ = db.Close()
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
	n := &notifier{b: meta.NewBroadcaster(watcher), pinned: conn, pinDB: db}
	n.version, _ = n.dataVersion()
	go n.b.Run(n.check, nil)
	return n, nil
}

func (n *notifier) stop() {
	n.b.Stop()
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
	err := n.pinned.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&v)
	return v, err
}
