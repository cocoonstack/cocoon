package hypervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gofrsflock "github.com/gofrs/flock"

	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/storage"
	storejson "github.com/cocoonstack/cocoon/storage/json"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	recordsDirName  = "vms"
	namesDirName    = "names"
	recordSuffix    = ".json"
	namesLockName   = "names.lock"
	orphanDirsName  = "orphan-dirs.json"
	orphanDirsLock  = "orphan-dirs.lock"
	migratedSuffix  = ".migrated"
	claimTmpPrefix  = ".claim-"
	birthRetryDelay = 2 * time.Millisecond
)

// VMDB stores one record file + flock per VM under db/vms so operations on
// different VMs never contend on a shared file or lock (the old monolithic
// vms.json serialized every CLI subprocess on one flock and rewrote the whole
// index per update). Name uniqueness comes from O_EXCL claim files under
// db/names instead of a locked map. db/vms.lock survives as the GC barrier:
// GC cycles hold it exclusive, record birth holds it shared (WithBirthShared).
type VMDB struct {
	recordsDir string
	namesDir   string
	// namesLock serializes only claim repair/release/sweep; the happy-path claim is a bare O_EXCL create.
	namesLock lock.Locker
	// barrierPath is the legacy index lock file, reused as the shared-mode side of the GC barrier.
	barrierPath string
	orphans     storage.Store[orphanDirIndex]
}

// orphanDirIndex persists migrated-dir cleanup intents (formerly VMIndex.OrphanDirs).
type orphanDirIndex struct {
	Dirs []string `json:"dirs,omitempty"`
}

// nameClaim is the content of a db/names/<sha256(name)>.json claim file; the
// name is hashed for filename safety and stored inside to detect collisions.
type nameClaim struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// OpenVMDB opens the per-VM record store next to the legacy index paths and
// runs the one-shot vms.json split if a legacy index is present; locker is the
// flock on indexLock and serializes the migration across processes.
func OpenVMDB(indexFile, indexLock string, locker lock.Locker) (*VMDB, error) {
	dbDir := filepath.Dir(indexFile)
	db := &VMDB{
		recordsDir:  filepath.Join(dbDir, recordsDirName),
		namesDir:    filepath.Join(dbDir, namesDirName),
		namesLock:   flock.New(filepath.Join(dbDir, namesLockName)),
		barrierPath: indexLock,
		orphans: storejson.New[orphanDirIndex](
			filepath.Join(dbDir, orphanDirsName), flock.New(filepath.Join(dbDir, orphanDirsLock))),
	}
	if err := utils.EnsureDirs(db.recordsDir, db.namesDir); err != nil {
		return nil, err
	}
	if err := db.migrateLegacy(indexFile, locker); err != nil {
		return nil, fmt.Errorf("migrate legacy index %s: %w", indexFile, err)
	}
	return db, nil
}

// RecordsDir is the directory holding one file per VM; watching it observes every record write and delete.
func (db *VMDB) RecordsDir() string { return db.recordsDir }

func (db *VMDB) recordPath(id string) string { return filepath.Join(db.recordsDir, id+recordSuffix) }

func (db *VMDB) recordLockPath(id string) string { return db.recordPath(id) + ".lock" }

// recordStore wraps one VM's file in the shared JSON store (atomic write, fsync
// discipline, .prev crash recovery). The lock is transient so deleted VMs leave
// no residue and delete cannot split a waiter onto a stale inode.
func (db *VMDB) recordStore(id string) *storejson.Store[VMRecord] {
	return storejson.New[VMRecord](db.recordPath(id), flock.NewTransient(db.recordLockPath(id)))
}

// validRecordID rejects refs that cannot be generated IDs (path metacharacters),
// so user refs never resolve to paths outside the records dir.
func validRecordID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `./\`)
}

// Get returns a copy of id's record; ok=false when none exists. Lock-free:
// records are replaced by atomic rename, so any read sees a complete generation.
func (db *VMDB) Get(id string) (VMRecord, bool, error) {
	if !validRecordID(id) {
		return VMRecord{}, false, nil
	}
	var rec VMRecord
	if err := db.recordStore(id).ReadRaw(func(r *VMRecord) error {
		rec = *r
		return nil
	}); err != nil {
		return VMRecord{}, false, err
	}
	// Written records always carry their ID, so a zero ID means the file is absent.
	return rec, rec.ID != "", nil
}

// List returns every record, lock-free (readdir + per-file reads); records
// deleted mid-scan are skipped.
func (db *VMDB) List() ([]*VMRecord, error) {
	ids, err := db.listIDs()
	if err != nil {
		return nil, err
	}
	recs := make([]*VMRecord, 0, len(ids))
	for _, id := range ids {
		rec, ok, err := db.Get(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		recs = append(recs, &rec)
	}
	return recs, nil
}

func (db *VMDB) listIDs() ([]string, error) {
	entries, err := os.ReadDir(db.recordsDir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, recordSuffix) {
			continue
		}
		// Filters .prev/.lock/temp siblings: real IDs never contain dots.
		if id := strings.TrimSuffix(name, recordSuffix); validRecordID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Update runs a locked read-modify-write on id's record and persists it
// (parent-dir fsync included); nothing is written when the record is missing —
// the error then matches errors.Is(_, ErrNotFound).
func (db *VMDB) Update(ctx context.Context, id string, fn func(*VMRecord) error) error {
	return db.recordStore(id).Update(ctx, func(r *VMRecord) error {
		if r.ID == "" {
			return fmt.Errorf("vm %s disappeared from index: %w", id, ErrNotFound)
		}
		return fn(r)
	})
}

// UpdateAny is Update admitting a missing record: fn sees exists=false with a
// zero record it may populate (record birth). No parent-dir fsync — a birth
// lost to power failure only re-exposes resources the GC orphan sweep reclaims,
// and FinalizeCreate's dir-synced write makes the entry durable.
func (db *VMDB) UpdateAny(ctx context.Context, id string, fn func(r *VMRecord, exists bool) error) error {
	return db.recordStore(id).UpdateNoDirSync(ctx, func(r *VMRecord) error {
		return fn(r, r.ID != "")
	})
}

// Put unconditionally writes rec under its lock without touching name claims;
// migration and tests seed records with it.
func (db *VMDB) Put(ctx context.Context, rec *VMRecord) error {
	if rec.ID == "" {
		return fmt.Errorf("refuse to store a record without ID")
	}
	return db.recordStore(rec.ID).Update(ctx, func(r *VMRecord) error {
		*r = *rec
		return nil
	})
}

// Delete removes id's record under its lock; a non-nil pred aborts the removal
// by returning false (deleted=false, no error). Returns the removed record and
// ErrNotFound when none exists.
func (db *VMDB) Delete(ctx context.Context, id string, pred func(*VMRecord) bool) (VMRecord, bool, error) {
	l := flock.NewTransient(db.recordLockPath(id))
	if err := l.Lock(ctx); err != nil {
		return VMRecord{}, false, err
	}
	defer func() { _ = l.Unlock(ctx) }()
	rec, ok, err := db.Get(id)
	if err != nil {
		return VMRecord{}, false, err
	}
	if !ok {
		return VMRecord{}, false, ErrNotFound
	}
	if pred != nil && !pred(&rec) {
		return rec, false, nil
	}
	main := db.recordPath(id)
	for _, p := range []string{main, main + storejson.PrevSuffix, main + storejson.PrevSuffix + ".tmp"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return rec, false, err
		}
	}
	// Dir fsync: a crash must not resurrect the record (the delete's caller has already torn resources down).
	return rec, true, utils.SyncParentDir(db.recordsDir)
}

// Resolve resolves ref (exact ID, name, or ID prefix ≥3 chars) with the same
// precedence as the legacy in-memory index, now over one claim read or a
// records readdir.
func (db *VMDB) Resolve(ref string) (string, error) {
	if _, ok, err := db.Get(ref); err != nil {
		return "", err
	} else if ok {
		return ref, nil
	}
	if c, ok, err := db.readClaim(ref); err != nil {
		return "", err
	} else if ok && c.Name == ref {
		if _, exists, err := db.Get(c.ID); err != nil {
			return "", err
		} else if exists {
			return c.ID, nil
		}
	}
	if len(ref) < 3 || !validRecordID(ref) {
		return "", ErrNotFound
	}
	return db.resolvePrefix(ref)
}

func (db *VMDB) resolvePrefix(ref string) (string, error) {
	ids, err := db.listIDs()
	if err != nil {
		return "", err
	}
	var match string
	for _, id := range ids {
		if !strings.HasPrefix(id, ref) {
			continue
		}
		if match != "" {
			return "", fmt.Errorf("ambiguous ref %q: multiple matches", ref)
		}
		match = id
	}
	if match == "" {
		return "", ErrNotFound
	}
	return match, nil
}

// ResolveMany batch-resolves refs to exact IDs, deduplicating results.
func (db *VMDB) ResolveMany(refs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(refs))
	var ids []string
	for _, ref := range refs {
		id, err := db.Resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", ref, err)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// WithBirthShared runs fn holding the GC barrier in shared mode: concurrent
// births proceed in parallel while a GC cycle (exclusive holder) sees no
// records appear mid-cycle — the invariant that keeps its pin/ownership
// snapshot trustworthy. lock/flock has no shared mode, hence gofrs directly.
func (db *VMDB) WithBirthShared(ctx context.Context, fn func() error) error {
	fl := gofrsflock.New(db.barrierPath)
	ok, err := fl.TryRLockContext(ctx, birthRetryDelay)
	if err != nil {
		_ = fl.Close()
		return fmt.Errorf("acquire birth barrier: %w", err)
	}
	if !ok {
		_ = fl.Close()
		return fmt.Errorf("acquire birth barrier: %w", ctx.Err())
	}
	defer fl.Close() //nolint:errcheck
	return fn()
}

func (db *VMDB) claimPath(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(db.namesDir, hex.EncodeToString(sum[:])+recordSuffix)
}

// readClaim reports (claim, true) for a decodable claim; a torn one (its
// writer died mid-claim, before any record write) reads as absent.
func (db *VMDB) readClaim(name string) (nameClaim, bool, error) {
	raw, err := os.ReadFile(db.claimPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nameClaim{}, false, nil
	}
	if err != nil {
		return nameClaim{}, false, err
	}
	var c nameClaim
	if json.Unmarshal(raw, &c) != nil || c.ID == "" {
		return nameClaim{}, false, nil
	}
	return c, true, nil
}

// claimAlive reports whether name's claim exists and its id still has a record.
func (db *VMDB) claimAlive(name string) (nameClaim, bool, error) {
	c, ok, err := db.readClaim(name)
	if err != nil || !ok {
		return c, false, err
	}
	_, exists, err := db.Get(c.ID)
	return c, exists, err
}

// ClaimName claims name→id via O_CREATE|O_EXCL, so concurrent same-name
// creates serialize on the filesystem instead of a shared lock. On EEXIST a
// live claim is a real conflict; a dead one (crashed create/delete) is
// repaired so names free up immediately, not after a GC grace.
func (db *VMDB) ClaimName(ctx context.Context, name, id string) error {
	err := db.tryClaim(name, id)
	if err == nil || !errors.Is(err, fs.ErrExist) {
		return err
	}
	return db.contendClaim(ctx, name, id)
}

// tryClaim publishes the claim by hardlinking a fully written temp file into
// place: link is create-exclusive like O_EXCL but atomic to readers, so a
// concurrent lookup can never observe a half-written claim and mistake it for
// a dead one.
func (db *VMDB) tryClaim(name, id string) error {
	data, err := json.Marshal(nameClaim{Name: name, ID: id})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(db.namesDir, claimTmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck
	_, werr := tmp.Write(data)
	// Claim durable before the record can be: name uniqueness must not split-brain across power loss.
	if err := errors.Join(werr, tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := os.Link(tmpPath, db.claimPath(name)); err != nil {
		return err
	}
	return utils.SyncParentDir(db.namesDir)
}

// contendClaim resolves an EEXIST. namesLock serializes repairs so two
// contenders cannot both unlink the same dead claim; a repair that misjudges an
// in-flight create (its claim→record window looks dead) self-heals through the
// owner's post-write VerifyClaim, which rolls the losing record back.
func (db *VMDB) contendClaim(ctx context.Context, name, id string) error {
	if c, live, err := db.claimAlive(name); err != nil {
		return err
	} else if live {
		return claimConflictErr(c, name)
	}
	if err := db.namesLock.Lock(ctx); err != nil {
		return err
	}
	defer func() { _ = db.namesLock.Unlock(ctx) }()
	c, live, err := db.claimAlive(name)
	if err != nil {
		return err
	}
	if live {
		return claimConflictErr(c, name)
	}
	if err := os.Remove(db.claimPath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := db.tryClaim(name, id); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// A fresh claimer slipped in between our unlink and link; it wins.
			if c, ok, readErr := db.readClaim(name); readErr != nil {
				return readErr
			} else if ok {
				return claimConflictErr(c, name)
			}
			// Winner already released again — vanishingly rare; report the conflict without an owner.
			return fmt.Errorf("vm name %q already exists", name)
		}
		return err
	}
	return nil
}

func claimConflictErr(c nameClaim, name string) error {
	if c.Name != name {
		return fmt.Errorf("vm name %q hash-collides with existing name %q", name, c.Name)
	}
	return fmt.Errorf("vm name %q already exists (id: %s)", name, c.ID)
}

// VerifyClaim confirms name still maps to id after the record write; owner is
// the current holder when it does not (see contendClaim).
func (db *VMDB) VerifyClaim(name, id string) (owner string, ok bool, err error) {
	c, exists, err := db.readClaim(name)
	if err != nil {
		return "", false, err
	}
	if !exists || c.ID != id {
		return c.ID, false, nil
	}
	return id, true, nil
}

// ReleaseName drops name's claim iff it maps to id; namesLock keeps the
// check-then-unlink atomic against a concurrent repair re-pointing the claim.
// Missing or foreign claims are left alone. No dir fsync: a crash-resurrected
// claim is dead (no record) and the next same-name create repairs it.
func (db *VMDB) ReleaseName(ctx context.Context, name, id string) error {
	c, ok, err := db.readClaim(name)
	if err != nil || !ok || c.ID != id {
		return err
	}
	if lockErr := db.namesLock.Lock(ctx); lockErr != nil {
		return lockErr
	}
	defer func() { _ = db.namesLock.Unlock(ctx) }()
	if c, ok, err = db.readClaim(name); err != nil || !ok || c.ID != id {
		return err
	}
	if rmErr := os.Remove(db.claimPath(name)); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return rmErr
	}
	return nil
}

// SweepDeadClaim removes the names-dir entry fileName if it is torn or its id
// has no record; the caller age-gates candidates so an in-flight create's
// claim→record window is never raided. Returns the claimed name when removed.
func (db *VMDB) SweepDeadClaim(ctx context.Context, fileName string) (removed bool, name string, err error) {
	path := filepath.Join(db.namesDir, fileName)
	dead, name, err := db.claimDead(path)
	if err != nil || !dead {
		return false, "", err
	}
	if lockErr := db.namesLock.Lock(ctx); lockErr != nil {
		return false, "", lockErr
	}
	defer func() { _ = db.namesLock.Unlock(ctx) }()
	if dead, name, err = db.claimDead(path); err != nil || !dead {
		return false, "", err
	}
	if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return false, "", rmErr
	}
	return true, name, nil
}

// claimDead reads a claim by path (the sweep walks filenames, not names) and
// reports whether it is reclaimable: torn, or pointing at a recordless id.
func (db *VMDB) claimDead(path string) (dead bool, name string, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, fs.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	var c nameClaim
	if json.Unmarshal(raw, &c) != nil || c.ID == "" {
		return true, c.Name, nil
	}
	_, exists, err := db.Get(c.ID)
	if err != nil {
		return false, "", err
	}
	return !exists, c.Name, nil
}

// OrphanDirs returns the standing migrated-dir cleanup intents (lock-free read).
func (db *VMDB) OrphanDirs() ([]string, error) {
	var dirs []string
	err := db.orphans.ReadRaw(func(idx *orphanDirIndex) error {
		dirs = slices.Clone(idx.Dirs)
		return nil
	})
	return dirs, err
}

// AddOrphanDirs persists cleanup intents, deduplicated.
func (db *VMDB) AddOrphanDirs(ctx context.Context, dirs []string) error {
	return db.orphans.Update(ctx, func(idx *orphanDirIndex) error {
		for _, d := range dirs {
			if !slices.Contains(idx.Dirs, d) {
				idx.Dirs = append(idx.Dirs, d)
			}
		}
		return nil
	})
}

// ClearOrphanDirs removes fulfilled cleanup intents.
func (db *VMDB) ClearOrphanDirs(ctx context.Context, dirs []string) error {
	return db.orphans.Update(ctx, func(idx *orphanDirIndex) error {
		idx.Dirs = slices.DeleteFunc(idx.Dirs, func(d string) bool { return slices.Contains(dirs, d) })
		return nil
	})
}

// migrateLegacy splits a monolithic vms.json into per-VM records, name claims
// and the orphan-dirs store, then renames it to vms.json.migrated (kept as the
// rollback backup). The legacy flock serializes concurrent new binaries — the
// loser re-checks and finds the file gone — and every entry write is an
// idempotent overwrite, so a crashed migration simply re-runs on next start.
func (db *VMDB) migrateLegacy(legacyPath string, locker lock.Locker) error {
	if _, err := os.Stat(legacyPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// Init-time: runs before any request context exists (same stance as storage/json's internal reads).
	ctx := context.Background()
	if err := locker.Lock(ctx); err != nil {
		return err
	}
	defer func() { _ = locker.Unlock(ctx) }()
	if _, err := os.Stat(legacyPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var legacy VMIndex
	// ReadRaw for the .prev crash recovery; we already hold the index flock.
	if err := storejson.New[VMIndex](legacyPath, locker).ReadRaw(func(idx *VMIndex) error {
		legacy = *idx
		return nil
	}); err != nil {
		return err
	}
	for id, rec := range legacy.VMs {
		if rec == nil || !validRecordID(id) {
			continue
		}
		if rec.ID == "" {
			rec.ID = id // Get infers existence from a non-empty ID
		}
		if err := utils.AtomicWriteJSON(db.recordPath(id), rec); err != nil {
			return err
		}
	}
	for name, id := range legacy.Names {
		if legacy.VMs[id] == nil {
			continue // dead mapping: legacy Resolve ignored it too
		}
		if err := utils.AtomicWriteJSON(db.claimPath(name), nameClaim{Name: name, ID: id}); err != nil {
			return err
		}
	}
	if len(legacy.OrphanDirs) > 0 {
		if err := db.AddOrphanDirs(ctx, legacy.OrphanDirs); err != nil {
			return err
		}
	}
	if err := os.Rename(legacyPath, legacyPath+migratedSuffix); err != nil {
		return err
	}
	return utils.SyncParentDir(filepath.Dir(legacyPath))
}
