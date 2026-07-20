package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/utils"
)

// Init creates a fresh store: schema DDL, identity pragmas and one
// initialized meta_state row per namespace, all in ONE transaction — a crash
// mid-init leaves nothing or a complete store (§6). A file with no
// meta_state table at all is a failed init and is restarted; anything else
// is left for the operator.
func Init(dbPath string, namespaces ...Namespace) error {
	if err := RefuseManifest(dbPath); err != nil {
		return err
	}
	return initStore(dbPath, namespaces)
}

// InitForRecovery is Init without the manifest guard — the conversion tool
// creates its target while the manifest is necessarily present (§6).
func InitForRecovery(dbPath string, namespaces ...Namespace) error {
	return initStore(dbPath, namespaces)
}

func initStore(dbPath string, namespaces []Namespace) (err error) {
	if merr := os.MkdirAll(filepath.Dir(dbPath), 0o750); merr != nil {
		return merr
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
	tx, err := db.Begin()
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := createSchema(tx, namespaces); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapErr(err)
	}
	// Identity pragmas cannot run inside the transaction; they land after the
	// schema commit, and failedInit treats their absence as a failed init.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA application_id = %d", ApplicationID)); err != nil {
		return mapErr(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", UserVersion)); err != nil {
		return mapErr(err)
	}
	return nil
}

func createSchema(tx *sql.Tx, namespaces []Namespace) error {
	if _, err := tx.Exec(`CREATE TABLE meta_state (
		namespace TEXT NOT NULL PRIMARY KEY, state TEXT NOT NULL,
		source TEXT, sha256 TEXT, records INTEGER, applied_at TEXT)`); err != nil {
		return mapErr(err)
	}
	for _, ns := range namespaces {
		for _, tbl := range ns.Tables {
			if _, err := tx.Exec("CREATE TABLE " + tableName(ns.Name, tbl) + " (id TEXT NOT NULL PRIMARY KEY, data TEXT NOT NULL)"); err != nil {
				return mapErr(err)
			}
		}
		if _, err := tx.Exec("INSERT INTO meta_state (namespace, state, records, applied_at) VALUES (?, 'initialized', 0, datetime('now'))", ns.Name); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// failedInit reports whether dbPath is a crashed init: identity pragmas
// unset or the meta_state table missing entirely.
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
	var tables, metaState int
	if serr := db.QueryRow("SELECT count(*), count(*) FILTER (WHERE name = 'meta_state') FROM sqlite_master WHERE type = 'table'").Scan(&tables, &metaState); serr != nil {
		return false, mapErr(serr)
	}
	if appID == 0 {
		// Our crash windows leave either an empty file or the committed schema
		// with pragmas unset; a populated zero-appid DB without meta_state is
		// someone else's file, never deleted.
		if tables == 0 || metaState > 0 {
			return true, nil
		}
		return false, fmt.Errorf("%s: populated database without cocoon identity; move it aside: %w", dbPath, meta.ErrCorrupt)
	}
	if appID != ApplicationID {
		return false, fmt.Errorf("%s: application_id %#x is not a cocoon meta store: %w", dbPath, appID, meta.ErrCorrupt)
	}
	return metaState == 0, nil
}
