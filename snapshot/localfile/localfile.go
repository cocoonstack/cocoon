package localfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"syscall"
	"time"

	gofrsflock "github.com/gofrs/flock"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/storage"
	storejson "github.com/cocoonstack/cocoon/storage/json"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	typ = "localfile"

	// leaseRetryDelay is the shared-lease poll cadence; contended only while a delete briefly holds the exclusive lock.
	leaseRetryDelay = 2 * time.Millisecond

	rollbackTimeout = 30 * time.Second
)

var (
	_ snapshot.Snapshot           = (*LocalFile)(nil)
	_ snapshot.Direct             = (*LocalFile)(nil)
	_ snapshot.DirectCreator      = (*LocalFile)(nil)
	_ snapshot.CompressedExporter = (*LocalFile)(nil)
	_ snapshot.DirectoryExporter  = (*LocalFile)(nil)

	osRename = os.Rename // seam for EXDEV fallback tests
)

// Option configures a LocalFile constructed via New.
type Option func(*LocalFile)

// WithGCPolicy attaches an LRU eviction policy used by RegisterGC.
func WithGCPolicy(p EvictionPolicy) Option {
	return func(lf *LocalFile) { lf.gcPolicy = p }
}

// LocalFile is the local-filesystem snapshot backend.
type LocalFile struct {
	conf     *Config
	store    storage.Store[snapshot.SnapshotIndex]
	locker   lock.Locker
	metering metering.Recorder
	gcPolicy EvictionPolicy
}

// New builds a LocalFile snapshot backend; rec may be nil and falls back to NopRecorder on emit.
func New(conf *config.Config, rec metering.Recorder, opts ...Option) (*LocalFile, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := NewConfig(conf)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	if rec == nil {
		rec = metering.NopRecorder{}
	}
	locker := flock.New(cfg.IndexLock())
	store := storejson.New[snapshot.SnapshotIndex](cfg.IndexFile(), locker)
	lf := &LocalFile{conf: cfg, store: store, locker: locker, metering: rec}
	for _, opt := range opts {
		opt(lf)
	}
	return lf, nil
}

func (lf *LocalFile) Type() string { return typ }

// DataDir returns the local data directory, snapshot config, and a read-lease release for direct file access.
func (lf *LocalFile) DataDir(ctx context.Context, ref string) (string, types.SnapshotConfig, func(), error) {
	rec, err := lf.lookupRecord(ctx, ref, true)
	if err != nil {
		return "", types.SnapshotConfig{}, nil, err
	}
	release, err := lf.acquireReadLease(ctx, rec.ID)
	if err != nil {
		return "", types.SnapshotConfig{}, nil, err
	}
	// Re-verify under the lease: a delete may have completed between lookup and acquire.
	if _, err := lf.lookupRecord(ctx, rec.ID, false); err != nil {
		release()
		return "", types.SnapshotConfig{}, nil, err
	}
	return rec.DataDir, snapshotRecordToConfig(rec), release, nil
}

// acquireBuildLease exclusively leases id while its data dir is being built (Create/Import), so rm/GC cannot resolve-and-delete the half-written dir.
func (lf *LocalFile) acquireBuildLease(id string) (func(), error) {
	fl := gofrsflock.New(lf.conf.LeasePath(id))
	locked, err := fl.TryLock()
	if err != nil {
		_ = fl.Close()
		return nil, fmt.Errorf("lease snapshot %s: %w", id, err)
	}
	if !locked {
		_ = fl.Close()
		return nil, fmt.Errorf("snapshot %s is in use", id)
	}
	return func() { _ = fl.Close() }, nil
}

// acquireReadLease holds a shared flock on the snapshot's lease file so delete/GC (exclusive) cannot reap the data dir mid-read; lock/flock has no shared mode, hence gofrs directly.
func (lf *LocalFile) acquireReadLease(ctx context.Context, id string) (func(), error) {
	fl := gofrsflock.New(lf.conf.LeasePath(id))
	ok, err := fl.TryRLockContext(ctx, leaseRetryDelay)
	if err != nil {
		_ = fl.Close()
		return nil, fmt.Errorf("lease snapshot %s: %w", id, err)
	}
	if !ok {
		_ = fl.Close()
		return nil, fmt.Errorf("lease snapshot %s: %w", id, ctx.Err())
	}
	return func() { _ = fl.Close() }, nil
}

// Create stores a snapshot via placeholder→extract→finalize; a mid-flight crash leaves a pending record for GC.
func (lf *LocalFile) Create(ctx context.Context, cfg *types.SnapshotConfig, stream io.Reader) (_ string, err error) {
	if cfg.ID == "" {
		return "", fmt.Errorf("snapshot ID is required (must be set by caller)")
	}
	release, err := lf.acquireBuildLease(cfg.ID)
	if err != nil {
		return "", err
	}
	defer release()
	dataDir, err := lf.beginCreate(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dataDir) //nolint:errcheck,gosec
			lf.rollbackCreate(ctx, cfg.ID, cfg.Name)
		}
	}()

	if err = os.MkdirAll(dataDir, 0o750); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	if err = utils.ExtractTar(dataDir, stream); err != nil {
		return "", fmt.Errorf("extract snapshot data: %w", err)
	}
	if err = lf.finalizeCreate(ctx, cfg, dataDir); err != nil {
		return "", err
	}
	return cfg.ID, nil
}

// CreateFromDir persists a snapshot by renaming srcDir into the store data dir — one atomic move, no tar read+extract — when they share a filesystem. It reports ok=false (srcDir untouched) across filesystems so the caller streams instead. The placeholder→move→finalize protocol matches Create, so a mid-flight crash leaves a GC-collectable pending record and the layout is byte-for-byte what Create produces.
func (lf *LocalFile) CreateFromDir(ctx context.Context, cfg *types.SnapshotConfig, srcDir string) (_ string, _ bool, err error) {
	sameFS, err := utils.SameFilesystem(srcDir, lf.conf.DataDir())
	if err != nil {
		return "", false, fmt.Errorf("check filesystem: %w", err)
	}
	if !sameFS {
		return "", false, nil
	}

	if cfg.ID == "" {
		return "", false, fmt.Errorf("snapshot ID is required (must be set by caller)")
	}
	release, err := lf.acquireBuildLease(cfg.ID)
	if err != nil {
		return "", false, err
	}
	defer release()
	dataDir, err := lf.beginCreate(ctx, cfg)
	if err != nil {
		return "", false, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dataDir) //nolint:errcheck,gosec
			lf.rollbackCreate(ctx, cfg.ID, cfg.Name)
		}
	}()

	// A st_dev match can't rule out EXDEV (bind mounts): roll back and report ok=false so the caller streams via tar.
	if err = osRename(srcDir, dataDir); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			lf.rollbackCreate(ctx, cfg.ID, cfg.Name)
			return "", false, nil
		}
		return "", false, fmt.Errorf("move capture into store: %w", err)
	}
	if err = os.Chmod(dataDir, 0o750); err != nil { //nolint:gosec // match the store's 0o750 data dirs (MkdirAll in Create)
		return "", false, fmt.Errorf("chmod data dir: %w", err)
	}
	// Durable before kill: the VMM dies once we return and a bare rename fsyncs nothing, so sync files and dir entries before finalize (the tar path fsyncs per file in ExtractTar).
	if err = utils.SyncTree(dataDir); err != nil {
		return "", false, fmt.Errorf("sync snapshot to disk: %w", err)
	}
	if err = lf.finalizeCreate(ctx, cfg, dataDir); err != nil {
		return "", false, err
	}
	return cfg.ID, true, nil
}

// List returns all snapshots (excluding pending ones).
func (lf *LocalFile) List(ctx context.Context) ([]*types.Snapshot, error) {
	var result []*types.Snapshot
	return result, lf.store.With(ctx, func(idx *snapshot.SnapshotIndex) error {
		for _, rec := range idx.Snapshots {
			if rec == nil || rec.Pending {
				continue
			}
			s := rec.Snapshot
			result = append(result, &s)
		}
		return nil
	})
}

func (lf *LocalFile) Inspect(ctx context.Context, ref string) (*types.Snapshot, error) {
	rec, err := lf.lookupRecord(ctx, ref, false)
	if err != nil {
		return nil, err
	}
	s := rec.Snapshot
	return &s, nil
}

// Delete removes each ref (rm dir → DB update); a mid-loop rm-OK-then-DB-fail leaves a stale DB record for GC.
func (lf *LocalFile) Delete(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	if err := lf.store.With(ctx, func(idx *snapshot.SnapshotIndex) error {
		var resolveErr error
		ids, resolveErr = idx.ResolveMany(refs)
		return resolveErr
	}); err != nil {
		return nil, err
	}

	var deleted []string
	for _, id := range ids {
		if err := lf.deleteOne(ctx, id); err != nil {
			return deleted, err
		}
		deleted = append(deleted, id)
	}
	return deleted, nil
}

func (lf *LocalFile) Restore(ctx context.Context, ref string) (types.SnapshotConfig, io.ReadCloser, error) {
	dataDir, cfg, release, err := lf.DataDir(ctx, ref)
	if err != nil {
		return types.SnapshotConfig{}, nil, err
	}
	return cfg, utils.TarDirStream(dataDir, release), nil
}

func (lf *LocalFile) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, gcModule(lf.conf, lf.store, lf.locker, lf.gcPolicy, lf.metering))
}

// deleteOne is idempotent under concurrent rm; the rival's emit is skipped so the ledger keeps exactly one stop per snapshot.
func (lf *LocalFile) deleteOne(ctx context.Context, id string) error {
	fl := gofrsflock.New(lf.conf.LeasePath(id))
	locked, err := fl.TryLock()
	if err != nil {
		return fmt.Errorf("lease snapshot %s: %w", id, err)
	}
	if !locked {
		_ = fl.Close()
		return fmt.Errorf("snapshot %s is in use by an active clone/restore/export", id)
	}
	defer fl.Close() //nolint:errcheck
	if err := os.RemoveAll(lf.conf.SnapshotDataDir(id)); err != nil {
		return fmt.Errorf("remove data dir %s: %w", id, err)
	}
	var (
		hypType       string
		deletedRecord bool
	)
	if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		rec := idx.Snapshots[id]
		if rec == nil {
			return nil
		}
		deletedRecord = true
		hypType = rec.Hypervisor
		if rec.Name != "" {
			delete(idx.Names, rec.Name)
		}
		delete(idx.Snapshots, id)
		return nil
	}); err != nil {
		return fmt.Errorf("delete DB record %s: %w", id, err)
	}
	if deletedRecord {
		emitSnapStop(ctx, lf.metering, id, hypType)
	}
	_ = os.Remove(lf.conf.LeasePath(id))
	return nil
}

// beginCreate validates cfg and inserts the pending placeholder record, returning the data dir.
func (lf *LocalFile) beginCreate(ctx context.Context, cfg *types.SnapshotConfig) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	dataDir := lf.conf.SnapshotDataDir(cfg.ID)
	if err := lf.insertRecord(ctx, cfg.ID, cfg.Name, &snapshot.SnapshotRecord{
		Snapshot: types.Snapshot{SnapshotConfig: *cfg, CreatedAt: time.Now()},
		Pending:  true,
		DataDir:  dataDir,
	}); err != nil {
		return "", err
	}
	return dataDir, nil
}

// finalizeCreate measures dataDir, flips the pending record durable, and emits metering.
func (lf *LocalFile) finalizeCreate(ctx context.Context, cfg *types.SnapshotConfig, dataDir string) error {
	size, err := utils.DirSize(dataDir)
	if err != nil {
		return fmt.Errorf("compute data dir size: %w", err)
	}
	finalizedAt := time.Now()
	if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		rec := idx.Snapshots[cfg.ID]
		if rec == nil {
			return fmt.Errorf("snapshot %q disappeared from index", cfg.ID)
		}
		rec.Pending = false
		rec.SizeBytes = size
		rec.LastAccessedAt = finalizedAt
		return nil
	}); err != nil {
		return fmt.Errorf("finalize snapshot: %w", err)
	}
	emitSnapStart(ctx, lf.metering, cfg.ID, cfg.Hypervisor, size, finalizedAt)
	return nil
}

// insertRecord adds rec under id with name-collision check; both Create (Pending) and Import (finalized) go through here.
func (lf *LocalFile) insertRecord(ctx context.Context, id, name string, rec *snapshot.SnapshotRecord) error {
	return lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		// Reject an existing ID: a same-ID retry must not adopt (and later roll back) a finalized snapshot.
		if idx.Snapshots[id] != nil {
			return fmt.Errorf("snapshot id %q already exists", id)
		}
		if name != "" {
			if existingID, ok := idx.Names[name]; ok {
				return fmt.Errorf("snapshot name %q already in use by %s", name, existingID)
			}
		}
		idx.Snapshots[id] = rec
		if name != "" {
			idx.Names[name] = id
		}
		return nil
	})
}

// rollbackCreate removes a placeholder snapshot record from the DB.
func (lf *LocalFile) rollbackCreate(ctx context.Context, id, name string) {
	// Survives Ctrl-C: a pending record left behind blocks same-name retries until GC.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		delete(idx.Snapshots, id)
		if name != "" && idx.Names[name] == id {
			delete(idx.Names, name)
		}
		return nil
	}); err != nil {
		log.WithFunc("localfile.rollbackCreate").Warnf(ctx, "rollback snapshot %s (name=%s): %v", id, name, err)
	}
}

// lookupRecord resolves ref to a non-pending record; touch=true also bumps LastAccessedAt under the same write lock.
func (lf *LocalFile) lookupRecord(ctx context.Context, ref string, touch bool) (snapshot.SnapshotRecord, error) {
	var rec snapshot.SnapshotRecord
	apply := func(idx *snapshot.SnapshotIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		r := idx.Snapshots[id]
		if r == nil || r.Pending {
			return snapshot.ErrNotFound
		}
		if touch {
			r.LastAccessedAt = time.Now()
		}
		rec = *r
		return nil
	}
	if touch {
		return rec, lf.store.Update(ctx, apply)
	}
	return rec, lf.store.With(ctx, apply)
}

// snapshotRecordToConfig builds a detached SnapshotConfig from a record, deep-copying ImageBlobIDs so the caller can use it after the lock is released.
func snapshotRecordToConfig(rec snapshot.SnapshotRecord) types.SnapshotConfig {
	cfg := rec.SnapshotConfig
	cfg.ImageBlobIDs = maps.Clone(rec.ImageBlobIDs)
	return cfg
}
