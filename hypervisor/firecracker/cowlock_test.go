package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/types"
)

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

// A lock acquired after a concurrent rm sees the record gone and must abort.
func TestEnsureSourceAliveAbortsOnDeletedSource(t *testing.T) {
	fc := newTestFC(t)
	const srcID = "SRCVM"
	srcDir := fc.Conf.VMRunDir(srcID)
	configs := []*types.StorageConfig{
		{Path: filepath.Join(srcDir, "cow.raw"), Role: types.StorageRoleCOW},
	}

	if err := fc.ensureSourceAlive(t.Context(), configs); err == nil {
		t.Fatal("expected error for missing source record")
	}

	if err := fc.ReserveVM(t.Context(), srcID, &types.VMConfig{Name: "src"}, nil, srcDir, fc.Conf.VMLogDir(srcID)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := fc.ensureSourceAlive(t.Context(), configs); err != nil {
		t.Fatalf("live source rejected: %v", err)
	}
}

func TestEnsureSourceAliveSkipsWithoutWritableDisks(t *testing.T) {
	fc := newTestFC(t)
	configs := []*types.StorageConfig{{Path: "/some/layer.erofs", Role: types.StorageRoleLayer}}
	if err := fc.ensureSourceAlive(t.Context(), configs); err != nil {
		t.Fatalf("layer-only configs must not require a record: %v", err)
	}
}

// Lock files live next to the disk they guard, so vm rm reclaims them with the run dir.
func TestCloneLockLivesNextToDisk(t *testing.T) {
	cow := filepath.Join(t.TempDir(), "cow.raw")
	called := false
	if err := withCOWPathLocked(t.Context(), cow, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if !called {
		t.Fatal("fn not called")
	}
	if _, err := os.Stat(cow + ".clone.lock"); err != nil {
		t.Fatalf("lock file not beside the disk: %v", err)
	}
}
