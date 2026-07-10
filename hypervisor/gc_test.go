package hypervisor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/types"
)

func TestGCCollectKeepsLockedVM(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "vm-gc-lock"
	runDir, logDir := t.TempDir(), t.TempDir()
	seedVMRecord(t, b, id, 1, 512, 1024, false)
	if err := b.DB.Update(ctx, func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateCreating
		idx.VMs[id].UpdatedAt = time.Now().Add(-25 * time.Hour)
		idx.VMs[id].RunDir = runDir
		idx.VMs[id].LogDir = logDir
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	l := flock.New(filepath.Join(runDir, OpsLockName))
	if ok, err := l.TryLock(ctx); err != nil || !ok {
		t.Fatalf("hold ops lock: ok=%v err=%v", ok, err)
	}
	snap := VMGCSnapshot{reasons: map[string]string{id: "stale-creating"}}
	if err := b.gcCollect(ctx, []string{id}, snap); err != nil {
		t.Fatalf("gcCollect (locked): %v", err)
	}
	if _, err := b.LoadRecord(ctx, id); err != nil {
		t.Fatal("record must survive while the ops lock is held (ownership kept)")
	}

	if err := l.Unlock(ctx); err != nil {
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
