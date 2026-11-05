package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/cocoonstack/cocoon/meta"
)

// Events pins one connection; data_version moves only for other connections' commits.
func (s *Store) Events(ctx context.Context) (<-chan struct{}, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notifier == nil {
		n, err := newNotifier(s.path)
		if err != nil {
			return nil, nil, err
		}
		s.notifier = n
	}
	ch, release := s.notifier.b.Subscribe()
	stop := context.AfterFunc(ctx, release)
	return ch, func() { stop(); release() }, nil
}

// Version is touched only by the init call and the Run goroutine.
type notifier struct {
	b       *meta.Broadcaster
	watcher *fsnotify.Watcher // kept for the severed-watch test seam
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
	n := &notifier{b: meta.NewBroadcaster(watcher), watcher: watcher, pinned: conn, pinDB: db}
	n.version, _ = n.dataVersion()
	go n.b.Run(n.check)
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
