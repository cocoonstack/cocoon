package hypervisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/lock/vmlock"
	"github.com/cocoonstack/cocoon/types"
)

func TestGCCollectKeepsLockedVM(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-gc-lock"
	runDir, logDir := t.TempDir(), t.TempDir()
	seedVMRecord(t, b, id, 1, 512, 1024, false)

	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateCreating
		idx.VMs[id].RunDir = runDir
		idx.VMs[id].LogDir = logDir
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	lk, err := vmlock.New(b.Conf.RootDirPath(), id)
	if err != nil {
		t.Fatalf("vm lock: %v", err)
	}
	if ok, err := lk.TryLock(ctx); err != nil || !ok {
		t.Fatalf("hold ops lock: ok=%v err=%v", ok, err)
	}
	snap := VMGCSnapshot{reasons: map[string]string{id: "stale-creating"}}
	if err := b.gcCollect(ctx, []string{id}, snap); err != nil {
		t.Fatalf("gcCollect (locked): %v", err)
	}
	if _, err := b.LoadRecord(ctx, id); err != nil {
		t.Fatal("record must survive while the ops lock is held (ownership kept)")
	}

	if err := lk.Unlock(ctx); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := b.gcCollect(ctx, []string{id}, snap); err != nil {
		t.Fatalf("gcCollect (unlocked): %v", err)
	}
	if _, err := b.LoadRecord(ctx, id); err == nil {
		t.Fatal("record must be unrecorded after a full reclaim")
	}
}

func TestWithOpsTryLockRecreatesMissingRunDir(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	dir := filepath.Join(t.TempDir(), "gone")
	ran := false
	if !b.withOpsTryLock(t.Context(), dir, func() { ran = true }) || !ran {
		t.Fatal("a missing run dir (logDir-only orphan) must not read as busy")
	}
}

func TestQuarantineVMSurvivesCanceledContext(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	const id = "vm-quarantine-cancel"
	seedVMRecord(t, b, id, 1, 512, 1024, true)

	cctx, cancel := context.WithCancel(t.Context())
	cancel()
	b.QuarantineVM(cctx, id, "test-reason")

	rec, err := b.LoadRecord(t.Context(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rec.Quarantine != "test-reason" || rec.State != types.VMStateError {
		t.Fatalf("quarantine lost under canceled ctx: quarantine=%q state=%s", rec.Quarantine, rec.State)
	}
}

func TestSweepStaleCaptureDirsCoversPersistedRunDir(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	oldRunDir := t.TempDir()
	stale := filepath.Join(oldRunDir, restoreStagingName)
	if err := os.Mkdir(stale, 0o750); err != nil {
		t.Fatalf("mk staging: %v", err)
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("age staging: %v", err)
	}

	snap := VMGCSnapshot{recRunDirs: []string{oldRunDir}}
	for _, err := range b.sweepStaleCaptureDirs(t.Context(), snap.sweepDirs(b.Conf.RunDir())) {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale staging in a migrated run dir must be reclaimed")
	}
}

func TestSweepDirsSkipsReservedNames(t *testing.T) {
	snap := VMGCSnapshot{runDirs: []string{"db", CloneLocksDirName, "SOMEVM"}}
	got := snap.sweepDirs("/root")
	want := []string{"/root/SOMEVM"}
	if !slices.Equal(got, want) {
		t.Fatalf("sweepDirs = %v, want %v", got, want)
	}
}

func TestSweepOrphanDirsReclaimsMigratedLeftover(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	leftover := t.TempDir()
	if err := os.WriteFile(filepath.Join(leftover, "cow.raw"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := b.dbUpdate(ctx, func(idx *VMIndex) error {
		idx.OrphanDirs = append(idx.OrphanDirs, leftover)
		return nil
	}); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	for _, err := range b.sweepOrphanDirs(ctx, []string{leftover}) {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatal("migrated leftover dir must be reclaimed")
	}
	if err := b.dbRead(t.Context(), func(idx *VMIndex) error {
		if len(idx.OrphanDirs) != 0 {
			t.Fatalf("intent must be cleared, got %v", idx.OrphanDirs)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}
