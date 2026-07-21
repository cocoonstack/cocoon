package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/utils"
)

// testBackupStep injects crashes between backup steps (§9 gate); nil in prod.
var testBackupStep func(step string) error

// MarkConverted records a namespace's conversion provenance in meta_state (§6).
func MarkConverted(ctx context.Context, dbPath, ns, source, sha256 string, records int) error {
	return withDB(dbPath, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, "UPDATE meta_state SET state = 'converted', source = ?, sha256 = ?, records = ?, applied_at = datetime('now') WHERE namespace = ?", source, sha256, records, ns)
		return mapErr(err)
	})
}

// Checkpoint folds the WAL back into the main file (TRUNCATE) so the
// database is a single self-contained file (§6 aside rule).
func Checkpoint(ctx context.Context, dbPath string) error {
	return withDB(dbPath, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		return mapErr(err)
	})
}

// Backup replaces destPath with a consistent single-file copy: VACUUM INTO
// a temp file, integrity-check, fsync, atomic rename, parent-dir sync (§4).
// A previously published backup stays intact until the rename commits (§9).
func Backup(ctx context.Context, dbPath, destPath string) (err error) {
	if merr := os.MkdirAll(filepath.Dir(destPath), 0o750); merr != nil {
		return merr
	}
	// Concurrent backups to one destination share the tmp path; without
	// mutual exclusion one run's cleanup yanks the other's tmp mid-verify.
	l := flock.New(destPath + ".lock")
	if lerr := l.Lock(ctx); lerr != nil {
		return lerr
	}
	defer func() { err = errors.Join(err, l.Unlock(ctx)) }()
	tmp := destPath + ".tmp"
	// A stale temp from a crashed run would block VACUUM INTO; the published
	// backup is untouched, so clearing it is safe.
	if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return rerr
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := withDB(dbPath, func(db *sql.DB) error {
		_, verr := db.ExecContext(ctx, "VACUUM INTO ?", tmp)
		return mapErr(verr)
	}); err != nil {
		return err
	}
	if err := backupStep("vacuumed"); err != nil {
		return err
	}
	if err := withDB(tmp, func(db *sql.DB) error {
		var result string
		if qerr := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); qerr != nil {
			return mapErr(qerr)
		}
		if result != "ok" {
			return fmt.Errorf("backup integrity check: %s", result)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := backupStep("verified"); err != nil {
		return err
	}
	if err := utils.SyncFile(tmp); err != nil {
		return err
	}
	if err := backupStep("synced"); err != nil {
		return err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		return err
	}
	if err := backupStep("renamed"); err != nil {
		return err
	}
	return utils.SyncParentDir(filepath.Dir(destPath))
}

func backupStep(step string) error {
	if testBackupStep == nil {
		return nil
	}
	return testBackupStep(step)
}

func withDB(dbPath string, fn func(*sql.DB) error) (err error) {
	db, err := open(dbPath, "FULL", true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	return fn(db)
}
