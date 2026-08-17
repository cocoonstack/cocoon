package sqlite

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/utils"
)

func TestInitRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	if err := Init(t.Context(), path, testDecls()...); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Init(t.Context(), path, testDecls()...); err == nil || !strings.Contains(err.Error(), "refusing to reinitialize") {
		t.Fatalf("want reinit refusal, got %v", err)
	}
}

func TestFailedInitRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Init(t.Context(), path, testDecls()...); err != nil {
		t.Fatalf("init over crashed init: %v", err)
	}
	if _, err := Open(path, testDecls()...); err != nil {
		t.Fatalf("open: %v", err)
	}
}

func TestInitIdentityAtomicWithSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	// The retired pragmas-after-commit window: schema present, identity absent. It must refuse loudly, never strand or silently delete.
	db, err := open(path, "FULL", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE meta_state (namespace TEXT NOT NULL PRIMARY KEY, state TEXT NOT NULL, source TEXT, sha256 TEXT, records INTEGER, applied_at TEXT)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := Init(t.Context(), path, testDecls()...); err == nil || !strings.Contains(err.Error(), "move it aside") {
		t.Fatalf("want loud refusal on half-identified db, got %v", err)
	}
}

func TestForeignPopulatedDBRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	db, err := open(path, "FULL", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE somebody_elses (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := Init(t.Context(), path, testDecls()...); err == nil || !strings.Contains(err.Error(), "move it aside") {
		t.Fatalf("want foreign-db refusal, got %v", err)
	}
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
			if err := Init(t.Context(), path, testDecls()...); err != nil {
				t.Fatalf("init: %v", err)
			}
			db, err := open(path, "FULL", true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(tt.pragma); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			if _, err := Open(path, testDecls()...); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
		})
	}
}

func TestUninitializedNamespaceRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	if err := Init(t.Context(), path, testDecls()...); err != nil {
		t.Fatalf("init: %v", err)
	}
	extra := append(testDecls(), Namespace{Name: "late", Tables: []string{"records"}})
	if _, err := Open(path, extra...); err == nil || !strings.Contains(err.Error(), "uninitialized") {
		t.Fatalf("want uninitialized refusal, got %v", err)
	}
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
	s, err := Open(path, testDecls()...)
	if err != nil {
		t.Fatalf("open bootstrapped store: %v", err)
	}
	_ = s.Close()
	if utils.FileExists(filepath.Join(filepath.Dir(path), initLockName)) {
		t.Fatal("transient init lock left behind")
	}
}

func TestInitIfMissingRepairsCrashedInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InitIfMissing(t.Context(), path, testDecls()...); err != nil {
		t.Fatalf("bootstrap over crashed init: %v", err)
	}
	if _, err := Open(path, testDecls()...); err != nil {
		t.Fatalf("open repaired store: %v", err)
	}
}

func TestOpenDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	if _, err := Open(path, testDecls()...); err == nil || !strings.Contains(err.Error(), "meta init") {
		t.Fatalf("want missing-store refusal, got %v", err)
	}
	if utils.FileExists(path) {
		t.Fatal("Open created a file")
	}
}

func TestUnsupportedFSRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DBFileName)
	if err := Init(t.Context(), path, testDecls()...); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Setenv("COCOON_TEST_UNSUPPORTED_FS", "nfs")
	if _, err := Open(path, testDecls()...); err == nil || !strings.Contains(err.Error(), "unsupported filesystem") {
		t.Fatalf("open: want fs refusal, got %v", err)
	}
	fresh := filepath.Join(dir, "sub", DBFileName)
	if err := Init(t.Context(), fresh, testDecls()...); err == nil || !strings.Contains(err.Error(), "unsupported filesystem") {
		t.Fatalf("init: want fs refusal, got %v", err)
	}
	if utils.FileExists(fresh) {
		t.Fatal("init created WAL state on refused filesystem")
	}
}

func testDecls() []Namespace {
	return []Namespace{{Name: "vms", Tables: []string{"records", "names"}}}
}
