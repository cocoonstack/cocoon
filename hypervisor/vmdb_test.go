package hypervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// TestReserveVMConcurrentDistinct is the G-0133 intent: creates of different
// VMs share no lock and no file, so none may be lost or serialized into
// failure under concurrency.
func TestReserveVMConcurrentDistinct(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const n = 32
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("VMCONC%026d", i)
			cfg := &types.VMConfig{}
			cfg.Name = fmt.Sprintf("conc-%d", i)
			errs[i] = b.ReserveVM(ctx, id, cfg, nil, t.TempDir(), t.TempDir())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	recs, err := b.DB.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != n {
		t.Fatalf("got %d records, want %d (lost creates)", len(recs), n)
	}
	for i := range n {
		id, err := b.DB.Resolve(fmt.Sprintf("conc-%d", i))
		if err != nil || id != fmt.Sprintf("VMCONC%026d", i) {
			t.Fatalf("resolve conc-%d = %q, %v", i, id, err)
		}
	}
}

// TestReserveVMConcurrentSameName: exactly one of N same-name creates may win,
// and no loser may leave a record behind.
func TestReserveVMConcurrentSameName(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := &types.VMConfig{}
			cfg.Name = "highlander"
			errs[i] = b.ReserveVM(ctx, fmt.Sprintf("VMRACE%026d", i), cfg, nil, t.TempDir(), t.TempDir())
		}()
	}
	wg.Wait()
	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("loser %d: unexpected error %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d winners, want exactly 1: %v", winners, errs)
	}
	recs, err := b.DB.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (loser left a record)", len(recs))
	}
	id, err := b.DB.Resolve("highlander")
	if err != nil || id != recs[0].ID {
		t.Fatalf("resolve = %q, %v; want the winner %q", id, err, recs[0].ID)
	}
}

func TestReserveVMIDCollisionAndAdopt(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	const id = "VMCOLLIDE"
	cfg := &types.VMConfig{}
	cfg.Name = "col-a"
	if err := b.ReserveVM(ctx, id, cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// Same id + same creating name adopts, refreshing dirs.
	newRun := t.TempDir()
	if err := b.ReserveVM(ctx, id, cfg, nil, newRun, newRun); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rec, err := b.LoadRecord(ctx, id)
	if err != nil || rec.RunDir != newRun {
		t.Fatalf("adopt did not refresh dirs: %+v, %v", rec.RunDir, err)
	}

	// Same id, different name: hard id collision.
	other := &types.VMConfig{}
	other.Name = "col-b"
	if err := b.ReserveVM(ctx, id, other, nil, t.TempDir(), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "id collision") {
		t.Fatalf("err = %v, want id collision", err)
	}

	// Different id, same name: name collision names the standing owner.
	if err := b.ReserveVM(ctx, "VMCOLLIDE2", cfg, nil, t.TempDir(), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("already exists (id: %s)", id)) {
		t.Fatalf("err = %v, want name collision naming %s", err, id)
	}
}

// TestReserveVMRepairsDeadClaim: a claim left by a crashed create/delete must
// not block its name until GC — the next same-name create repairs it inline.
func TestReserveVMRepairsDeadClaim(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	if err := b.DB.ClaimName(ctx, "phoenix", "VMGONE"); err != nil {
		t.Fatalf("seed dead claim: %v", err)
	}
	cfg := &types.VMConfig{}
	cfg.Name = "phoenix"
	if err := b.ReserveVM(ctx, "VMHEIR", cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("reserve over dead claim: %v", err)
	}
	if id, err := b.DB.Resolve("phoenix"); err != nil || id != "VMHEIR" {
		t.Fatalf("resolve = %q, %v; want VMHEIR", id, err)
	}
}

func TestRollbackCreateFreesName(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	cfg := &types.VMConfig{}
	cfg.Name = "ephemeral"
	if err := b.ReserveVM(ctx, "VMROLLBACK", cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	b.RollbackCreate(ctx, "VMROLLBACK", "ephemeral")
	if _, err := b.LoadRecord(ctx, "VMROLLBACK"); err == nil {
		t.Fatal("record must be gone after rollback")
	}
	// The name must be reusable at once.
	if err := b.ReserveVM(ctx, "VMSECOND", cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("re-reserve rolled-back name: %v", err)
	}
}

func TestCleanStalePlaceholdersReleasesName(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	cfg := &types.VMConfig{}
	cfg.Name = "stale-name"
	if err := b.ReserveVM(ctx, "VMSTALE", cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := b.DB.Update(ctx, "VMSTALE", func(r *VMRecord) error {
		r.UpdatedAt = time.Now().Add(-25 * time.Hour)
		return nil
	}); err != nil {
		t.Fatalf("age placeholder: %v", err)
	}
	if err := b.CleanStalePlaceholders(ctx, []string{"VMSTALE"}); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := b.LoadRecord(ctx, "VMSTALE"); err == nil {
		t.Fatal("stale placeholder must be removed")
	}
	if err := b.ReserveVM(ctx, "VMFRESH", cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("name must be free after placeholder sweep: %v", err)
	}
}

// TestSweepOrphanNameClaims: old recordless claims are reclaimed, young ones
// (a possible in-flight create) and live ones are kept.
func TestSweepOrphanNameClaims(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	seedVMRecord(t, b, "VMLIVE", 1, 512, 1024, false)
	for name, id := range map[string]string{"old-orphan": "VMDEAD1", "young-orphan": "VMDEAD2", "live": "VMLIVE"} {
		if err := b.DB.ClaimName(ctx, name, id); err != nil {
			t.Fatalf("claim %s: %v", name, err)
		}
	}
	past := time.Now().Add(-25 * time.Hour)
	for _, name := range []string{"old-orphan", "live"} {
		if err := os.Chtimes(b.DB.claimPath(name), past, past); err != nil {
			t.Fatalf("age claim: %v", err)
		}
	}

	for _, err := range b.sweepOrphanNameClaims(ctx) {
		t.Fatalf("sweep: %v", err)
	}
	if _, ok, _ := b.DB.readClaim("old-orphan"); ok {
		t.Fatal("aged orphan claim must be swept")
	}
	if _, ok, _ := b.DB.readClaim("young-orphan"); !ok {
		t.Fatal("young claim must survive (possible in-flight create)")
	}
	if _, ok, _ := b.DB.readClaim("live"); !ok {
		t.Fatal("claim with a live record must survive")
	}
}

// TestMigrateLegacyIndex covers the one-shot vms.json split: records, name
// claims and orphan-dir intents carry over, the legacy file becomes the
// .migrated backup, and a second open is a no-op.
func TestMigrateLegacyIndex(t *testing.T) {
	dir := t.TempDir()
	conf := meteringStubConfig{
		stubBackendConfig: stubBackendConfig{
			indexFile: filepath.Join(dir, "vms.json"),
			indexLock: filepath.Join(dir, "vms.lock"),
		},
		vmRunRoot: dir,
	}
	mkRec := func(id, name string, state types.VMState) *VMRecord {
		r := &VMRecord{VM: types.VM{ID: id, State: state}}
		r.Config.Name = name
		return r
	}
	legacy := &VMIndex{
		VMs: map[string]*VMRecord{
			"VMA": mkRec("VMA", "alpha", types.VMStateRunning),
			"VMB": mkRec("VMB", "beta", types.VMStateStopped),
		},
		Names:      map[string]string{"alpha": "VMA", "beta": "VMB", "ghost": "VMGONE"},
		OrphanDirs: []string{"/nonexistent/legacy-dir"},
	}
	if err := utils.AtomicWriteJSON(conf.indexFile, legacy); err != nil {
		t.Fatalf("write legacy index: %v", err)
	}

	b, err := NewBackend("test-hv", conf, nil)
	if err != nil {
		t.Fatalf("NewBackend (migration): %v", err)
	}
	if _, statErr := os.Stat(conf.indexFile); !os.IsNotExist(statErr) {
		t.Fatal("legacy vms.json must be renamed away")
	}
	if _, statErr := os.Stat(conf.indexFile + migratedSuffix); statErr != nil {
		t.Fatalf("migrated backup missing: %v", statErr)
	}
	recs, err := b.DB.List()
	if err != nil || len(recs) != 2 {
		t.Fatalf("List = %d recs, %v; want 2", len(recs), err)
	}
	for name, id := range map[string]string{"alpha": "VMA", "beta": "VMB"} {
		if got, err := b.DB.Resolve(name); err != nil || got != id {
			t.Fatalf("resolve %s = %q, %v; want %s", name, got, err, id)
		}
	}
	if _, err := b.DB.Resolve("ghost"); err == nil {
		t.Fatal("dead legacy name mapping must not resolve")
	}
	dirs, err := b.DB.OrphanDirs()
	if err != nil || len(dirs) != 1 || dirs[0] != "/nonexistent/legacy-dir" {
		t.Fatalf("orphan dirs = %v, %v; want the legacy intent", dirs, err)
	}
	rec, err := b.LoadRecord(t.Context(), "VMA")
	if err != nil || rec.State != types.VMStateRunning {
		t.Fatalf("VMA = %+v, %v; want running", rec.State, err)
	}

	// Reopen: idempotent, nothing re-migrated.
	if _, err := NewBackend("test-hv", conf, nil); err != nil {
		t.Fatalf("NewBackend (reopen): %v", err)
	}
}

// TestDeleteFreesNameAndFiles: rm must leave no record, lock, .prev or claim
// residue behind, and the name must be immediately reusable.
func TestDeleteFreesNameAndFiles(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	ctx := t.Context()
	cfg := &types.VMConfig{}
	cfg.Name = "reusable"
	if err := b.ReserveVM(ctx, "VMDEL", cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Touch the record once so a .prev generation exists.
	if err := b.DB.Update(ctx, "VMDEL", func(r *VMRecord) error {
		r.State = types.VMStateStopped
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	gone, deleted, err := b.DB.Delete(ctx, "VMDEL", nil)
	if err != nil || !deleted || gone.ID != "VMDEL" {
		t.Fatalf("delete = %v/%v/%v", gone.ID, deleted, err)
	}
	if err := b.DB.ReleaseName(ctx, "reusable", "VMDEL"); err != nil {
		t.Fatalf("release: %v", err)
	}
	entries, err := os.ReadDir(b.DB.RecordsDir())
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "VMDEL") {
			t.Fatalf("residue after delete: %s", e.Name())
		}
	}
	if err := b.ReserveVM(ctx, "VMDEL2", cfg, nil, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("name must be reusable after delete: %v", err)
	}
}
