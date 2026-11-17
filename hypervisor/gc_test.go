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
	seedVMRecord(t, b, id, 1, 512, 1024, false)
	setVMState(t, b, id, types.VMStateCreating)

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

func TestQuarantineVMSurvivesCanceledContext(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	const id = "vm-quarantine-cancel"
	seedVMRecord(t, b, id, 1, 512, 1024, true)

	cctx, cancel := context.WithCancel(t.Context())
	cancel()
	b.quarantineVM(cctx, id, "test-reason")

	rec := recordOf(t, b, id)
	if rec.Quarantine != "test-reason" || rec.State != types.VMStateError {
		t.Fatalf("quarantine lost under canceled ctx: quarantine=%q state=%s", rec.Quarantine, rec.State)
	}
}

func TestSweepStaleCaptureDirsReclaimsAgedStaging(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	oldRunDir := t.TempDir()
	stale := filepath.Join(oldRunDir, restoreStagingName)
	if err := os.Mkdir(stale, 0o750); err != nil {
		t.Fatalf("mk staging: %v", err)
	}
	past := time.Now().Add(-StaleArtifactGCGrace - time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("age staging: %v", err)
	}

	for _, err := range b.sweepStaleCaptureDirs(t.Context(), []string{oldRunDir}) {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("aged staging must be reclaimed")
	}
}

func TestSweepStaleCloneLocksReportsAnUnreadableDir(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	if errs := b.sweepStaleCloneLocks(t.Context()); len(errs) != 0 {
		t.Fatalf("missing clone-locks dir: %v, want no error", errs)
	}
	if err := os.WriteFile(filepath.Join(b.Conf.RunDir(), CloneLocksDirName), nil, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if errs := b.sweepStaleCloneLocks(t.Context()); len(errs) != 1 {
		t.Fatalf("errs = %v, want the read failure", errs)
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
