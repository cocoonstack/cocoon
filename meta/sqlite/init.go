package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/utils"
)

const initLockName = "init.lock"

// Init creates a fresh store: schema DDL, identity pragmas and one
// initialized meta_state row per namespace, all in ONE transaction — a crash
// mid-init leaves nothing or a complete store (§6). An empty database is a
// failed init and is restarted; anything else is left for the operator.
func Init(ctx context.Context, dbPath string, namespaces ...Namespace) error {
	if err := RefuseManifest(dbPath); err != nil {
		return err
	}
	return initStore(ctx, dbPath, namespaces)
}

// InitForRecovery is Init without the manifest guard — the conversion tool
// creates its target while the manifest is necessarily present (§6).
func InitForRecovery(ctx context.Context, dbPath string, namespaces ...Namespace) error {
	return initStore(ctx, dbPath, namespaces)
}

// InitIfMissing bootstraps a fresh store, serializing racing processes
// behind a transient flock; an existing database is a no-op.
func InitIfMissing(ctx context.Context, dbPath string, namespaces ...Namespace) (err error) {
	if merr := os.MkdirAll(filepath.Dir(dbPath), 0o750); merr != nil {
		return merr
	}
	lk := flock.NewTransient(filepath.Join(filepath.Dir(dbPath), initLockName))
	if lerr := lk.Lock(ctx); lerr != nil {
		return lerr
	}
	defer func() { err = errors.Join(err, lk.Unlock(ctx)) }()
	if utils.FileExists(dbPath) {
		return nil
	}
	return Init(ctx, dbPath, namespaces...)
}

func initStore(ctx context.Context, dbPath string, namespaces []Namespace) (err error) {
	if merr := os.MkdirAll(filepath.Dir(dbPath), 0o750); merr != nil {
		return merr
	}
	if ferr := checkFS(dbPath); ferr != nil {
		return ferr
	}
	if utils.FileExists(dbPath) {
		partial, perr := failedInit(dbPath)
		if perr != nil {
			return perr
		}
		if !partial {
			return fmt.Errorf("%s already exists; refusing to reinitialize", dbPath)
		}
		if rerr := os.Remove(dbPath); rerr != nil {
			return rerr
		}
	}
	db, err := open(dbPath, "FULL", true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := createSchema(ctx, tx, namespaces); err != nil {
		return err
	}
	// Identity pragmas ride the same transaction as the schema: a crash
	// leaves nothing (or an empty file) or a complete store, never a
	// half-identified one (§6).
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", ApplicationID)); err != nil {
		return mapErr(err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", UserVersion)); err != nil {
		return mapErr(err)
	}
	return mapErr(tx.Commit())
}

func createSchema(ctx context.Context, tx *sql.Tx, namespaces []Namespace) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE meta_state (
		namespace TEXT NOT NULL PRIMARY KEY, state TEXT NOT NULL,
		source TEXT, sha256 TEXT, records INTEGER, applied_at TEXT)`); err != nil {
		return mapErr(err)
	}
	for _, ns := range namespaces {
		for _, tbl := range ns.Tables {
			if _, err := tx.ExecContext(ctx, "CREATE TABLE "+tableName(ns.Name, tbl)+" (id TEXT NOT NULL PRIMARY KEY, data TEXT NOT NULL)"); err != nil {
				return mapErr(err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO meta_state (namespace, state, records, applied_at) VALUES (?, 'initialized', 0, datetime('now'))", ns.Name); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// failedInit reports whether dbPath is a crashed init. Init is atomic, so
// the only restartable state is an empty database (driver lazy-touch or a
// crash before the commit); anything populated is refused, never deleted.
func failedInit(dbPath string) (bool, error) {
	db, err := open(dbPath, "FULL", false)
	if err != nil {
		return false, err
	}
	defer db.Close() //nolint:errcheck
	var appID int64
	if serr := db.QueryRow("PRAGMA application_id").Scan(&appID); serr != nil {
		if strings.Contains(serr.Error(), "not a database") {
			return false, fmt.Errorf("%s is not a sqlite database: %w", dbPath, meta.ErrCorrupt)
		}
		return false, mapErr(serr)
	}
	var tables int
	if serr := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); serr != nil {
		return false, mapErr(serr)
	}
	if tables == 0 {
		return true, nil
	}
	if appID != ApplicationID {
		return false, fmt.Errorf("%s: populated database without cocoon identity; move it aside: %w", dbPath, meta.ErrCorrupt)
	}
	return false, nil
}
