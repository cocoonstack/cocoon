package sqlite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/utils"
)

func testDecls() []Namespace {
	return []Namespace{{Name: "vms", Tables: []string{"records", "names"}}}
}

func TestInitRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	if err := Init(path, testDecls()...); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Init(path, testDecls()...); err == nil || !strings.Contains(err.Error(), "refusing to reinitialize") {
		t.Fatalf("want reinit refusal, got %v", err)
	}
}

func TestFailedInitRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Init(path, testDecls()...); err != nil {
		t.Fatalf("init over crashed init: %v", err)
	}
	if _, err := Open(path, testDecls()...); err != nil {
		t.Fatalf("open: %v", err)
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
	if err := Init(path, testDecls()...); err == nil || !strings.Contains(err.Error(), "move it aside") {
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
			if err := Init(path, testDecls()...); err != nil {
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
	if err := Init(path, testDecls()...); err != nil {
		t.Fatalf("init: %v", err)
	}
	extra := append(testDecls(), Namespace{Name: "late", Tables: []string{"records"}})
	if _, err := Open(path, extra...); err == nil || !strings.Contains(err.Error(), "uninitialized") {
		t.Fatalf("want uninitialized refusal, got %v", err)
	}
}
