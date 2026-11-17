package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/utils"
)

func TestInitRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	mustInit(t, path)
	requireErrContains(t, Init(t.Context(), path, testDecls()...), "refusing to reinitialize")
}

func TestCrashedInitRepaired(t *testing.T) {
	for _, tt := range []struct {
		name string
		fn   func(context.Context, string, ...Namespace) error
	}{
		{"Init", Init},
		{"InitIfMissing", InitIfMissing},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), DBFileName)
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tt.fn(t.Context(), path, testDecls()...); err != nil {
				t.Fatalf("%s over crashed init: %v", tt.name, err)
			}
			mustOpen(t, path)
		})
	}
}

func TestInitIdentityAtomicWithSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)

	execRaw(t, path, "CREATE TABLE meta_state (namespace TEXT NOT NULL PRIMARY KEY, state TEXT NOT NULL, source TEXT, sha256 TEXT, records INTEGER, applied_at TEXT)")
	requireErrContains(t, Init(t.Context(), path, testDecls()...), "move it aside")
}

func TestForeignPopulatedDBRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	execRaw(t, path, "CREATE TABLE somebody_elses (id INTEGER PRIMARY KEY)")
	requireErrContains(t, Init(t.Context(), path, testDecls()...), "move it aside")
	if !utils.FileExists(path) {
		t.Fatal("foreign db was deleted")
	}
}

func TestIdentityFailClosed(t *testing.T) {
	for _, tt := range []struct {
		name, pragma, want string
	}{
		{"foreign app id", "PRAGMA application_id = 123", "not a cocoon meta store"},
		{"newer schema", "PRAGMA user_version = 99", "newer than this binary"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), DBFileName)
			mustInit(t, path)
			execRaw(t, path, tt.pragma)
			_, err := Open(path, testDecls()...)
			requireErrContains(t, err, tt.want)
		})
	}
}

func TestUninitializedNamespaceRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	mustInit(t, path)
	extra := append(testDecls(), Namespace{Name: "late", Tables: []string{"records"}})
	_, err := Open(path, extra...)
	requireErrContains(t, err, "uninitialized")
}

func TestInitIfMissingRaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			errs[i] = InitIfMissing(t.Context(), path, testDecls()...)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	if err := InitIfMissing(t.Context(), path, testDecls()...); err != nil {
		t.Fatalf("idempotent recheck: %v", err)
	}
	mustOpen(t, path)
	if utils.FileExists(filepath.Join(filepath.Dir(path), initLockName)) {
		t.Fatal("transient init lock left behind")
	}
}

func TestOpenRefusesOlderGeneration(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), DBFileName)
	mustInit(t, path)
	execRaw(t, path, fmt.Sprintf("PRAGMA user_version = %d", UserVersion-1))
	_, err := Open(path, testDecls()...)
	requireErrContains(t, err, "meta-upgrade.py")
	if err := InitIfMissing(ctx, path, testDecls()...); err != nil {
		t.Fatalf("bootstrap must pass an initialized store through untouched: %v", err)
	}
}

func TestOpenDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	_, err := Open(path, testDecls()...)
	requireErrContains(t, err, "meta init")
	if utils.FileExists(path) {
		t.Fatal("Open created a file")
	}
}

func TestUnsupportedFSRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DBFileName)
	mustInit(t, path)
	t.Setenv("COCOON_TEST_UNSUPPORTED_FS", "nfs")
	_, err := Open(path, testDecls()...)
	requireErrContains(t, err, "unsupported filesystem")
	fresh := filepath.Join(dir, "sub", DBFileName)
	requireErrContains(t, Init(t.Context(), fresh, testDecls()...), "unsupported filesystem")
	if utils.FileExists(fresh) {
		t.Fatal("init created WAL state on refused filesystem")
	}
}

func TestInitIfMissingWaitsOutABusyBootstrap(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), DBFileName)
	initLock := flock.NewTransient(filepath.Join(filepath.Dir(path), initLockName))
	if err := initLock.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	release := holdExclusive(t, path)
	done := make(chan error, 1)
	go func() { done <- InitIfMissing(ctx, path, testDecls()...) }()
	time.Sleep(500 * time.Millisecond)
	release()
	if err := initLock.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("bootstrap behind a busy racer: %v", err)
	}
	mustOpen(t, path)
}

func TestInitWaitsForTheInitLock(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), DBFileName)
	initLock := flock.NewTransient(filepath.Join(filepath.Dir(path), initLockName))
	if err := initLock.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Init(ctx, path, testDecls()...) }()
	select {
	case err := <-done:
		t.Fatalf("Init ran while a bootstrap held the init lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := initLock.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Init after the bootstrap released the lock: %v", err)
	}
	mustOpen(t, path)
}

func TestInitIfMissingRetriesABusyLockedRecheck(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), DBFileName)
	release := holdExclusive(t, path)
	done := make(chan error, 1)
	go func() { done <- InitIfMissing(ctx, path, testDecls()...) }()
	time.Sleep(300 * time.Millisecond)
	release()
	if err := <-done; err != nil {
		t.Fatalf("bootstrap whose locked recheck met a busy database: %v", err)
	}
	mustOpen(t, path)
}

func TestInitIfMissingRepairsInPlaceUnderAnOpenProbe(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), DBFileName)
	probe, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	conn, err := probe.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var tables int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("empty probe: tables=%d err=%v", tables, err)
	}
	if err := InitIfMissing(ctx, path, testDecls()...); err != nil {
		t.Fatalf("bootstrap over an empty store a probe holds open: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); err != nil || tables == 0 {
		t.Fatalf("probe after bootstrap: tables=%d err=%v, want the probe's file to be the bootstrapped store", tables, err)
	}
}

func testDecls() []Namespace {
	return []Namespace{{Name: "vms", Tables: []string{"records", "names"}}}
}

func mustInit(t *testing.T, path string) {
	t.Helper()
	if err := Init(t.Context(), path, testDecls()...); err != nil {
		t.Fatalf("init: %v", err)
	}
}

func mustOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, testDecls()...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func execRaw(t *testing.T, path, stmt string) {
	t.Helper()
	db, err := open(path, "FULL", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
}

func holdExclusive(t *testing.T, path string) (release func()) {
	t.Helper()
	ctx := t.Context()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		_ = raw.Close()
	}
}

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want error containing %q", err, want)
	}
}
