// Package sqlite is the meta scale engine: one WAL database, namespace =
// table group, record-granularity writes (design §2-§4). Physical schema is
// the generic (id, data) pair per (namespace, table) — the record SPI's
// shape — declared by the composition root exactly like the json engine's
// table codecs (v2.28).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	sqlite3 "modernc.org/sqlite"
	sqlite3lib "modernc.org/sqlite/lib"

	"github.com/cocoonstack/cocoon/meta"
)

const (
	// ApplicationID marks a cocoon DB ("COCN"); UserVersion is the schema
	// generation — verified on every open, written only at init (§6).
	ApplicationID = 0x434F434E
	UserVersion   = 1

	// DBFileName is the single database under the meta root.
	DBFileName = "meta.db"

	busyRetryCeiling = 5 * time.Second
	busyRetryBase    = 2 * time.Millisecond
	slowTxnWarn      = 500 * time.Millisecond
)

// Namespace declares one namespace's table set; Tables lists the record
// tables (satellites included) the SPI may address.
type Namespace struct {
	Name   string
	Tables []string
}

var _ meta.Store = (*Store)(nil)

// Store is the sqlite engine: writerDurable/writerRelaxed single-conn
// handles, a bounded reader pool, and a pinned notifier connection (§4).
type Store struct {
	path          string
	nss           map[string]Namespace
	writerDurable *sql.DB
	writerRelaxed *sql.DB
	readers       *sql.DB
	notifier      *notifier
}

// Open verifies identity, version and per-namespace meta_state, then builds
// the connection set. It never creates or migrates — that is Init's job.
func Open(dbPath string, namespaces ...Namespace) (*Store, error) {
	if err := refuseManifest(dbPath); err != nil {
		return nil, err
	}
	return openStore(dbPath, namespaces)
}

// OpenForRecovery is Open without the manifest guard, for the conversion
// tool itself (§6) — never for ordinary callers.
func OpenForRecovery(dbPath string, namespaces ...Namespace) (*Store, error) {
	return openStore(dbPath, namespaces)
}

func openStore(dbPath string, namespaces []Namespace) (*Store, error) {
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
	if err := s.verifyIdentity(); err != nil {
		return nil, errors.Join(err, s.Close())
	}
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
	return fn(&txHandle{ctx: ctx, tx: tx, store: s, scope: scopeSet(nss)})
}

func (s *Store) Update(ctx context.Context, sc meta.Scope, mode meta.CommitMode, fn func(meta.Writer) error) error {
	if sc.Write == "" {
		return fmt.Errorf("update requires a write namespace: %w", meta.ErrScope)
	}
	nss := append([]string{sc.Write}, sc.Read...)
	if err := s.checkScope(nss); err != nil {
		return err
	}
	writer := s.writerDurable
	if mode == meta.CommitRelaxed {
		writer = s.writerRelaxed
	}
	start := time.Now()
	tx, err := s.beginImmediate(ctx, writer)
	if err != nil {
		return err
	}
	h := &txHandle{ctx: ctx, tx: tx, store: s, scope: scopeSet(nss), write: sc.Write, mode: mode}
	if err := fn(h); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return mapErr(err)
	}
	if d := time.Since(start); d > slowTxnWarn {
		log.WithFunc("meta.sqlite.Update").Warnf(ctx, "slow transaction on %s: %s", sc.Write, d)
	}
	return nil
}

// Close stops the notifier and closes every handle.
func (s *Store) Close() error {
	var errs []error
	if s.notifier != nil {
		s.notifier.stop()
		s.notifier = nil
	}
	for _, db := range []*sql.DB{s.writerDurable, s.writerRelaxed, s.readers} {
		if db != nil {
			errs = append(errs, db.Close())
		}
	}
	s.writerDurable, s.writerRelaxed, s.readers = nil, nil, nil
	return errors.Join(errs...)
}

// beginImmediate retries BEGIN IMMEDIATE under a ctx-bounded jittered loop:
// the short in-driver busy_timeout alone would sleep past caller deadlines
// (clause 6), and a persistent writer must surface ErrBusy, not hang.
func (s *Store) beginImmediate(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	deadline := time.Now().Add(busyRetryCeiling)
	backoff := busyRetryBase
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
		jitter := time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff + jitter):
		}
		backoff = min(backoff*2, 100*time.Millisecond)
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

// verifyIdentity reads application_id, user_version and meta_state — never
// writes them (§6): wrong id or a newer version fails closed; a namespace
// with no meta_state row is uninitialized, never empty.
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

func tableName(ns, table string) string {
	return quoteIdent(ns + "__" + table)
}

// quoteIdent quotes a SQL identifier; namespace and table names come from
// the composition root, never user input, but quoting keeps them inert.
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

func open(dbPath, sync string, writer bool) (*sql.DB, error) {
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(50)&_pragma=foreign_keys(1)&_pragma=trusted_schema(0)&_pragma=synchronous(" + sync + ")"
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

func isBusy(err error) bool {
	var se *sqlite3.Error
	if errors.As(err, &se) {
		code := se.Code() & 0xff
		return code == sqlite3lib.SQLITE_BUSY || code == sqlite3lib.SQLITE_LOCKED
	}
	return false
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite3.Error
	if !errors.As(err, &se) {
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

// txHandle implements Reader/Writer over one transaction; values are
// detached by construction (every read allocates from row scans).
type txHandle struct {
	ctx   context.Context
	tx    *sql.Tx
	store *Store
	scope map[string]struct{}
	write string
	mode  meta.CommitMode
}

var (
	_ meta.Reader = (*txHandle)(nil)
	_ meta.Writer = (*txHandle)(nil)
)

func (h *txHandle) GetRaw(ctx context.Context, ns, table, id string) (json.RawMessage, bool, error) {
	if err := h.checkRead(ns); err != nil {
		return nil, false, err
	}
	var data []byte
	err := h.tx.QueryRowContext(ctx, "SELECT data FROM "+tableName(ns, table)+" WHERE id = ?", id).Scan(&data)
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
	rows, err := h.tx.QueryContext(ctx, "SELECT id, data FROM "+tableName(ns, table)+" ORDER BY rowid")
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
	if err := h.checkWrite(ns, relaxedOK); err != nil {
		return err
	}
	_, err := h.tx.ExecContext(ctx, "INSERT INTO "+tableName(ns, table)+" (id, data) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data", id, []byte(raw))
	return mapErr(err)
}

func (h *txHandle) DeleteRaw(ctx context.Context, ns, table, id string, relaxedOK bool) error {
	if err := h.checkWrite(ns, relaxedOK); err != nil {
		return err
	}
	_, err := h.tx.ExecContext(ctx, "DELETE FROM "+tableName(ns, table)+" WHERE id = ?", id)
	return mapErr(err)
}

func (h *txHandle) Mode() meta.CommitMode { return h.mode }

func (h *txHandle) checkRead(ns string) error {
	if _, ok := h.scope[ns]; !ok {
		return fmt.Errorf("read %s: %w", ns, meta.ErrScope)
	}
	return nil
}

func (h *txHandle) checkWrite(ns string, relaxedOK bool) error {
	if ns != h.write {
		return fmt.Errorf("write %s: %w", ns, meta.ErrScope)
	}
	if h.mode == meta.CommitRelaxed && !relaxedOK {
		return fmt.Errorf("write %s: %w", ns, meta.ErrDurabilityContract)
	}
	return nil
}

// ManifestName marks an in-flight conversion at the meta root; ordinary
// opens refuse while it exists (§6).
const ManifestName = "meta-convert.manifest"

func refuseManifest(dbPath string) error {
	manifest := filepath.Join(filepath.Dir(dbPath), ManifestName)
	if exists(manifest) {
		return fmt.Errorf("%s exists: a conversion is in flight, run `cocoon meta convert` to finish it", manifest)
	}
	return nil
}
