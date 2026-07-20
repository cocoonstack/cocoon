package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocoonstack/cocoon/utils"
)

// MarkConverted records a namespace's conversion provenance in meta_state (§6).
func MarkConverted(dbPath, ns, source, sha256 string, records int) error {
	return withDB(dbPath, func(db *sql.DB) error {
		_, err := db.Exec("UPDATE meta_state SET state = 'converted', source = ?, sha256 = ?, records = ?, applied_at = datetime('now') WHERE namespace = ?", source, sha256, records, ns)
		return mapErr(err)
	})
}

// Checkpoint folds the WAL back into the main file (TRUNCATE) so the
// database is a single self-contained file (§6 aside rule).
func Checkpoint(dbPath string) error {
	return withDB(dbPath, func(db *sql.DB) error {
		_, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		return mapErr(err)
	})
}

// Backup produces a consistent single-file copy at destPath: VACUUM INTO a
// temp file, integrity-check it, fsync, then rename into place (§4).
func Backup(dbPath, destPath string) (err error) {
	if utils.FileExists(destPath) {
		return fmt.Errorf("%s already exists; refusing to overwrite", destPath)
	}
	if merr := os.MkdirAll(filepath.Dir(destPath), 0o750); merr != nil {
		return merr
	}
	tmp := destPath + ".tmp"
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := withDB(dbPath, func(db *sql.DB) error {
		_, verr := db.Exec("VACUUM INTO ?", tmp)
		return mapErr(verr)
	}); err != nil {
		return err
	}
	if err := withDB(tmp, func(db *sql.DB) error {
		var result string
		if qerr := db.QueryRow("PRAGMA integrity_check").Scan(&result); qerr != nil {
			return mapErr(qerr)
		}
		if result != "ok" {
			return fmt.Errorf("backup integrity check: %s", result)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := utils.SyncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		return err
	}
	return utils.SyncParentDir(filepath.Dir(destPath))
}

func withDB(dbPath string, fn func(*sql.DB) error) (err error) {
	db, err := open(dbPath, "FULL", true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	return fn(db)
}
