// Package sqlite is the meta scale engine: one WAL database, namespace = table group, generic (id, data) rows per root-declared table (§2-§4, v2.28).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	sqlite3 "modernc.org/sqlite"
	sqlite3lib "modernc.org/sqlite/lib"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// ApplicationID marks a cocoon DB ("COCN"); UserVersion is the schema generation — verified on every open, written only at init (§6).
	ApplicationID = 0x434F434E
	UserVersion   = 1

	// DBFileName is the single database under the meta root; ManifestName beside it marks an in-flight conversion, which ordinary opens refuse (§6).
	DBFileName   = "meta.db"
	ManifestName = "meta-convert.manifest"

	// busyRetryPause caps the jittered pause between BEGIN IMMEDIATE retries; the in-driver busy_timeout already did the real waiting (§4).
	busyRetryCeiling   = 5 * time.Second
	busyRetryPause     = 2 * time.Millisecond
	slowTxnWarn        = 500 * time.Millisecond
	checkpointInterval = time.Second
)

// Namespace declares one namespace's table set; Tables lists the record tables (satellites included) the SPI may address.
type Namespace struct {
	Name   string
	Tables []string
}

// tableStmts holds one table's prepared statements; scan/put/del are nil on read-only handles.
type tableStmts struct {
	get, scan, put, del *sql.Stmt
}

var _ meta.Store = (*Store)(nil)

// Store is the sqlite engine: writerDurable/writerRelaxed single-conn handles, a bounded reader pool, and a pinned notifier connection (§4). Statements are prepared per handle at Open — the table set is static.
type Store struct {
	path          string
	nss           map[string]Namespace
	writerDurable *sql.DB
	writerRelaxed *sql.DB
	readers       *sql.DB
	stmtsDurable  map[string]tableStmts
	stmtsRelaxed  map[string]tableStmts
	stmtsReaders  map[string]tableStmts
	cpDone        chan struct{}

	mu       sync.Mutex // guards notifier lazy-init against concurrent Events/Close
	notifier *notifier
}

// Open verifies identity, version and per-namespace meta_state, then builds the connection set. It never creates or migrates — that is Init's job.
func Open(dbPath string, namespaces ...Namespace) (*Store, error) {
	if err := RefuseManifest(dbPath); err != nil {
		return nil, err
	}
	return openStore(dbPath, namespaces)
}

// OpenForRecovery is Open without the manifest guard, for the conversion tool itself (§6) — never for ordinary callers.
func OpenForRecovery(dbPath string, namespaces ...Namespace) (*Store, error) {
	return openStore(dbPath, namespaces)
}

// RefuseManifest fails when a conversion manifest sits beside dbPath, meaning an offline conversion is unfinished.
func RefuseManifest(dbPath string) error {
	manifest := filepath.Join(filepath.Dir(dbPath), ManifestName)
	if utils.FileExists(manifest) {
		return fmt.Errorf("%s exists: a conversion is in flight, run `cocoon meta convert` to finish it", manifest)
	}
	return nil
}

func openStore(dbPath string, namespaces []Namespace) (*Store, error) {
	// The driver creates a file on first touch; Open never creates — that is Init's job (§6) — and §4 refuses network filesystems before WAL work.
	if !utils.FileExists(dbPath) {
		return nil, fmt.Errorf("no sqlite store at %s: run `cocoon meta init` or `cocoon meta convert`", dbPath)
	}
	if err := checkFS(dbPath); err != nil {
		return nil, err
	}
	s := &Store{path: dbPath, nss: map[string]Namespace{}}
	for _, ns := range namespaces {
		if ns.Name == "" || len(ns.Tables) == 0 {
			return nil, fmt.Errorf("namespace %q: incomplete declaration", ns.Name)
		}
		if _, ok := s.nss[ns.Name]; ok {
			return nil, fmt.Errorf("namespace %q declared twice", ns.Name)
		}
		s.nss[ns.Name] = ns
	}
	var err error
	if s.writerDurable, err = open(dbPath, "FULL", true); err != nil {
		return nil, err
	}
	if s.writerRelaxed, err = open(dbPath, "NORMAL", true); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if s.readers, err = open(dbPath, "FULL", false); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	s.readers.SetMaxOpenConns(max(2, runtime.NumCPU()))
	if err = s.verifyIdentity(); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if s.stmtsDurable, err = prepareStmts(s.writerDurable, s.nss, true); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if s.stmtsRelaxed, err = prepareStmts(s.writerRelaxed, s.nss, true); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if s.stmtsReaders, err = prepareStmts(s.readers, s.nss, false); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	// A background PASSIVE checkpoint keeps the WAL short so committing writers rarely pay the autocheckpoint stall themselves.
	s.cpDone = make(chan struct{})
	go checkpointLoop(s.readers, dbPath, s.cpDone)
	return s, nil
}

func (s *Store) View(ctx context.Context, nss []string, fn func(meta.Reader) error) error {
	if err := s.checkScope(nss); err != nil {
		return err
	}
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback() //nolint:errcheck
	return fn(&txHandle{ctx: ctx, tx: tx, sm: s.stmtsReaders, scope: scopeSet(nss)})
}

func (s *Store) Update(ctx context.Context, sc meta.Scope, mode meta.CommitMode, fn func(meta.Writer) error) error {
	if sc.Write == "" {
		return fmt.Errorf("update requires a write namespace: %w", meta.ErrScope)
	}
	nss := append([]string{sc.Write}, sc.Read...)
	if err := s.checkScope(nss); err != nil {
		return err
	}
	writer, sm := s.writerDurable, s.stmtsDurable
	if mode == meta.CommitRelaxed {
		writer, sm = s.writerRelaxed, s.stmtsRelaxed
	}
	start := time.Now()
	tx, err := s.beginImmediate(ctx, writer)
	if err != nil {
		return err
	}
	wait := time.Since(start)
	h := &txHandle{ctx: ctx, tx: tx, sm: sm, scope: scopeSet(nss), write: sc.Write, mode: mode}
	if err := fn(h); err != nil {
		_ = tx.Rollback()
		return err
	}
	commitStart := time.Now()
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return mapErr(err)
	}
	// §4 observability: writer wait, transaction and commit durations.
	commit, total := time.Since(commitStart), time.Since(start)
	logger := log.WithFunc("meta.sqlite.Update")
	if total > slowTxnWarn {
		logger.Warnf(ctx, "slow transaction on %s: total %s wait %s commit %s", sc.Write, total, wait, commit)
	} else {
		logger.Debugf(ctx, "txn %s: total %s wait %s commit %s", sc.Write, total, wait, commit)
	}
	return nil
}

func (s *Store) Close() error {
	var errs []error
	s.mu.Lock()
	if s.notifier != nil {
		s.notifier.stop()
		s.notifier = nil
	}
	s.mu.Unlock()
	if s.cpDone != nil {
		close(s.cpDone)
		s.cpDone = nil
	}
	for _, sm := range []map[string]tableStmts{s.stmtsDurable, s.stmtsRelaxed, s.stmtsReaders} {
		for _, ts := range sm {
			for _, st := range []*sql.Stmt{ts.get, ts.scan, ts.put, ts.del} {
				if st != nil {
					_ = st.Close()
				}
			}
		}
	}
	s.stmtsDurable, s.stmtsRelaxed, s.stmtsReaders = nil, nil, nil
	for _, db := range []*sql.DB{s.writerDurable, s.writerRelaxed, s.readers} {
		if db != nil {
			errs = append(errs, db.Close())
		}
	}
	s.writerDurable, s.writerRelaxed, s.readers = nil, nil, nil
	return errors.Join(errs...)
}

// beginImmediate retries BEGIN IMMEDIATE under a ctx-bounded loop: the short in-driver busy_timeout does the real waiting (and bounds ctx latency, clause 6), so between attempts only a tiny jittered pause bounds spin — an exponential backoff here would idle past a freed lock.
func (s *Store) beginImmediate(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	deadline := time.Now().Add(busyRetryCeiling)
	for {
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isBusy(err) {
			return nil, mapErr(err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("writer contended past retry ceiling: %w", meta.ErrBusy)
		}
		pause := time.Duration(rand.Int64N(int64(busyRetryPause))) //nolint:gosec // retry jitter, not cryptographic
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pause):
		}
	}
}

func (s *Store) checkScope(nss []string) error {
	for _, name := range nss {
		if _, ok := s.nss[name]; !ok {
			return fmt.Errorf("namespace %q: %w", name, meta.ErrScope)
		}
	}
	return nil
}

// verifyIdentity reads application_id, user_version and meta_state — never writes them (§6): wrong id or a newer version fails closed; a namespace with no meta_state row is uninitialized, never empty.
func (s *Store) verifyIdentity() error {
	var appID, version int64
	if err := s.readers.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		return mapErr(err)
	}
	if err := s.readers.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return mapErr(err)
	}
	if appID != ApplicationID {
		return fmt.Errorf("%s: application_id %#x is not a cocoon meta store: %w", s.path, appID, meta.ErrCorrupt)
	}
	if version > UserVersion {
		return fmt.Errorf("%s: schema version %d newer than this binary (%d); upgrade cocoon: %w", s.path, version, UserVersion, meta.ErrCorrupt)
	}
	for name := range s.nss {
		var state string
		err := s.readers.QueryRow("SELECT state FROM meta_state WHERE namespace = ?", name).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("namespace %q uninitialized (no meta_state row): run `cocoon meta init` or `cocoon meta convert`", name)
		}
		if err != nil {
			return mapErr(err)
		}
	}
	return nil
}

var (
	_ meta.Reader = (*txHandle)(nil)
	_ meta.Writer = (*txHandle)(nil)
)

// txHandle implements Reader/Writer over one transaction; values are detached by construction (every read allocates from row scans).
type txHandle struct {
	ctx   context.Context
	tx    *sql.Tx
	sm    map[string]tableStmts
	scope map[string]struct{}
	write string
	mode  meta.CommitMode
}

func (h *txHandle) GetRaw(ctx context.Context, ns, table, id string) (json.RawMessage, bool, error) {
	if err := h.checkRead(ns); err != nil {
		return nil, false, err
	}
	ts, err := h.stmts(ns, table)
	if err != nil {
		return nil, false, err
	}
	var data []byte
	err = h.tx.StmtContext(ctx, ts.get).QueryRowContext(ctx, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, mapErr(err)
	}
	return data, true, nil
}

func (h *txHandle) ScanRaw(ctx context.Context, ns, table string, fn func(id string, raw json.RawMessage) error) error {
	if err := h.checkRead(ns); err != nil {
		return err
	}
	ts, err := h.stmts(ns, table)
	if err != nil {
		return err
	}
	rows, err := h.tx.StmtContext(ctx, ts.scan).QueryContext(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return mapErr(err)
		}
		if err := fn(id, data); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (h *txHandle) PutRaw(ctx context.Context, ns, table, id string, raw json.RawMessage, relaxedOK bool) error {
	if err := meta.CheckWriteScope(ns, h.write, h.mode, relaxedOK); err != nil {
		return err
	}
	ts, err := h.stmts(ns, table)
	if err != nil {
		return err
	}
	_, err = h.tx.StmtContext(ctx, ts.put).ExecContext(ctx, id, []byte(raw))
	return mapErr(err)
}

func (h *txHandle) DeleteRaw(ctx context.Context, ns, table, id string, relaxedOK bool) error {
	if err := meta.CheckWriteScope(ns, h.write, h.mode, relaxedOK); err != nil {
		return err
	}
	ts, err := h.stmts(ns, table)
	if err != nil {
		return err
	}
	_, err = h.tx.StmtContext(ctx, ts.del).ExecContext(ctx, id)
	return mapErr(err)
}

func (h *txHandle) stmts(ns, table string) (tableStmts, error) {
	ts, ok := h.sm[ns+"\x00"+table]
	if !ok {
		return tableStmts{}, fmt.Errorf("table %s/%s not declared: %w", ns, table, meta.ErrScope)
	}
	return ts, nil
}

func (h *txHandle) checkRead(ns string) error {
	if _, ok := h.scope[ns]; !ok {
		return fmt.Errorf("read %s: %w", ns, meta.ErrScope)
	}
	return nil
}

func tableName(ns, table string) string {
	return quoteIdent(ns + "__" + table)
}

// quoteIdent keeps root-declared identifiers inert in SQL text.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func scopeSet(nss []string) map[string]struct{} {
	set := make(map[string]struct{}, len(nss))
	for _, ns := range nss {
		set[ns] = struct{}{}
	}
	return set
}

// open applies the per-connection runtime contract (§4); cache_size is KB (negative form) and mmap_size covers the whole file at meta scale.
func open(dbPath, sync string, writer bool) (*sql.DB, error) {
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(50)&_pragma=foreign_keys(1)&_pragma=trusted_schema(0)" +
		"&_pragma=cache_size(-16384)&_pragma=mmap_size(268435456)&_pragma=synchronous(" + sync + ")"
	if writer {
		dsn += "&_txlock=immediate"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, mapErr(err)
	}
	if writer {
		db.SetMaxOpenConns(1)
	}
	return db, nil
}

func prepareStmts(db *sql.DB, nss map[string]Namespace, writer bool) (map[string]tableStmts, error) {
	m := map[string]tableStmts{}
	for _, ns := range nss {
		for _, tbl := range ns.Tables {
			tn := tableName(ns.Name, tbl)
			var ts tableStmts
			var err error
			if ts.get, err = db.Prepare("SELECT data FROM " + tn + " WHERE id = ?"); err != nil { //nolint:gosec // table name via quoteIdent from root declarations
				return nil, mapErr(err)
			}
			if ts.scan, err = db.Prepare("SELECT id, data FROM " + tn + " ORDER BY rowid"); err != nil { //nolint:gosec // table name via quoteIdent from root declarations
				return nil, mapErr(err)
			}
			if writer {
				if ts.put, err = db.Prepare("INSERT INTO " + tn + " (id, data) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data"); err != nil { //nolint:gosec // table name via quoteIdent from root declarations
					return nil, mapErr(err)
				}
				if ts.del, err = db.Prepare("DELETE FROM " + tn + " WHERE id = ?"); err != nil { //nolint:gosec // table name via quoteIdent from root declarations
					return nil, mapErr(err)
				}
			}
			m[ns.Name+"\x00"+tbl] = ts
		}
	}
	return m, nil
}

func checkpointLoop(db *sql.DB, dbPath string, done <-chan struct{}) {
	// Detached engine goroutine: it outlives every caller ctx by design.
	ctx := context.Background()
	logger := log.WithFunc("meta.sqlite.checkpointLoop")
	t := time.NewTicker(checkpointInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			start := time.Now()
			var busy, frames, moved int
			if err := db.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &frames, &moved); err != nil || frames == 0 {
				continue
			}
			// §4 observability: WAL bytes and checkpoint duration.
			var walBytes int64
			if st, serr := os.Stat(dbPath + "-wal"); serr == nil {
				walBytes = st.Size()
			}
			logger.Debugf(ctx, "passive checkpoint: %d/%d frames, wal %dB, %s", moved, frames, walBytes, time.Since(start))
		}
	}
}

func isBusy(err error) bool {
	if se, ok := errors.AsType[*sqlite3.Error](err); ok {
		code := se.Code() & 0xff
		return code == sqlite3lib.SQLITE_BUSY || code == sqlite3lib.SQLITE_LOCKED
	}
	return false
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	se, ok := errors.AsType[*sqlite3.Error](err)
	if !ok {
		return err
	}
	switch se.Code() & 0xff {
	case sqlite3lib.SQLITE_BUSY, sqlite3lib.SQLITE_LOCKED:
		return fmt.Errorf("%v: %w", err, meta.ErrBusy)
	case sqlite3lib.SQLITE_CONSTRAINT:
		return fmt.Errorf("%v: %w", err, meta.ErrConflict)
	case sqlite3lib.SQLITE_CORRUPT, sqlite3lib.SQLITE_NOTADB:
		return fmt.Errorf("%v: %w", err, meta.ErrCorrupt)
	case sqlite3lib.SQLITE_FULL:
		return fmt.Errorf("%v: %w", err, meta.ErrNoSpace)
	case sqlite3lib.SQLITE_IOERR, sqlite3lib.SQLITE_CANTOPEN, sqlite3lib.SQLITE_READONLY:
		return fmt.Errorf("%v: %w", err, meta.ErrIO)
	default:
		return err
	}
}
