package firecracker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/types"
)

func TestCloneLockDeadSourceSucceeds(t *testing.T) {
	fc := newTestFC(t)
	srcDir := fc.Conf.VMRunDir("GONE")
	configs := []*types.StorageConfig{
		{Path: filepath.Join(srcDir, "cow.raw"), Role: types.StorageRoleCOW},
		{Path: filepath.Join(srcDir, "data-x.raw"), Role: types.StorageRoleData},
	}

	called := false
	if err := fc.withSourceWritableDisksLocked(t.Context(), configs, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("dead-source lock: %v", err)
	}
	if !called {
		t.Fatal("fn not called")
	}
	if _, err := os.Stat(srcDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source dir must not be resurrected by locking: %v", err)
	}
	assertNoLockResidue(t, fc.cloneLockDir())
}

func TestCloneLockHeldThenCleaned(t *testing.T) {
	fc := newTestFC(t)
	cow := filepath.Join(fc.Conf.VMRunDir("GONE"), "cow.raw")
	lockDir := fc.cloneLockDir()
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("setup lock dir: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withCOWPathLocked(t.Context(), lockDir, cow, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	entries, err := os.ReadDir(lockDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want exactly one lock file while held, got %d (%v)", len(entries), err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("locked fn: %v", err)
	}
	if err := withCOWPathLocked(t.Context(), lockDir, cow, func() error { return nil }); err != nil {
		t.Fatalf("relock after release: %v", err)
	}
	assertNoLockResidue(t, lockDir)
}

func newTestFC(t *testing.T) *Firecracker {
	t.Helper()
	dir := t.TempDir()
	fc, err := New(&config.Config{
		RootDir: dir,
		RunDir:  filepath.Join(dir, "run"),
		LogDir:  filepath.Join(dir, "log"),
	}, nil)
	if err != nil {
		t.Fatalf("new firecracker: %v", err)
	}
	return fc
}

func assertNoLockResidue(t *testing.T, lockDir string) {
	t.Helper()
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		t.Fatalf("read lock dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("lock residue after release: %v", entries)
	}
}
