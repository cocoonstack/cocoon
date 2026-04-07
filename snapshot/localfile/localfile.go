package localfile

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/storage"
	storejson "github.com/cocoonstack/cocoon/storage/json"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const typ = "localfile"

// compile-time interface checks.
var (
	_ snapshot.Snapshot           = (*LocalFile)(nil)
	_ snapshot.Direct             = (*LocalFile)(nil)
	_ snapshot.CompressedExporter = (*LocalFile)(nil)
)

// LocalFile implements snapshot.Snapshot using the local filesystem.
type LocalFile struct {
	conf   *Config
	store  storage.Store[snapshot.SnapshotIndex]
	locker lock.Locker
}

func (lf *LocalFile) Type() string { return typ }

// New creates a new LocalFile snapshot backend.
func New(conf *config.Config) (*LocalFile, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := NewConfig(conf)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	locker := flock.New(cfg.IndexLock())
	store := storejson.New[snapshot.SnapshotIndex](cfg.IndexFile(), locker)
	return &LocalFile{conf: cfg, store: store, locker: locker}, nil
}

// DataDir returns the local data directory and snapshot config for direct file access.
func (lf *LocalFile) DataDir(ctx context.Context, ref string) (string, *types.SnapshotConfig, error) {
	var (
		cfg     *types.SnapshotConfig
		dataDir string
	)
	return dataDir, cfg, lf.store.With(ctx, func(idx *snapshot.SnapshotIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.Snapshots[id]
		if rec == nil || rec.Pending {
			return snapshot.ErrNotFound
		}
		cfg = snapshotRecordToConfig(rec)
		dataDir = rec.DataDir
		return nil
	})
}

// Uses a two-phase pattern (placeholder → extract → finalize) so that
// a crash between phases leaves a pending record that GC will clean up,
// rather than an orphan data directory with no DB entry.
func (lf *LocalFile) Create(ctx context.Context, cfg *types.SnapshotConfig, stream io.Reader) (_ string, err error) {
	id := cfg.ID
	if id == "" {
		return "", fmt.Errorf("snapshot ID is required (must be set by caller)")
	}

	dataDir := lf.conf.SnapshotDataDir(id)
	now := time.Now()

	// Phase 1: write placeholder record so GC won't orphan our dir.
	if err = lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		if cfg.Name != "" {
			if existingID, ok := idx.Names[cfg.Name]; ok {
				return fmt.Errorf("snapshot name %q already in use by %s", cfg.Name, existingID)
			}
		}
		idx.Snapshots[id] = &snapshot.SnapshotRecord{
			Snapshot: types.Snapshot{
				SnapshotConfig: *cfg,
				CreatedAt:      now,
			},
			Pending: true,
			DataDir: dataDir,
		}
		if cfg.Name != "" {
			idx.Names[cfg.Name] = id
		}
		return nil
	}); err != nil {
		return "", err
	}

	// Rollback on failure: remove data dir + placeholder record.
	defer func() {
		if err != nil {
			os.RemoveAll(dataDir) //nolint:errcheck,gosec
			lf.rollbackCreate(ctx, id, cfg.Name)
		}
	}()

	// Phase 2: create dir + extract data.
	if err = os.MkdirAll(dataDir, 0o750); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	if err = utils.ExtractTar(dataDir, stream); err != nil {
		return "", fmt.Errorf("extract snapshot data: %w", err)
	}

	// Phase 3: finalize — clear pending flag.
	if err = lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		rec := idx.Snapshots[id]
		if rec == nil {
			return fmt.Errorf("snapshot %q disappeared from index", id)
		}
		rec.Pending = false
		return nil
	}); err != nil {
		return "", fmt.Errorf("finalize snapshot: %w", err)
	}

	return id, nil
}

// rollbackCreate removes a placeholder snapshot record from the DB.
func (lf *LocalFile) rollbackCreate(ctx context.Context, id, name string) {
	if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		delete(idx.Snapshots, id)
		if name != "" {
			delete(idx.Names, name)
		}
		return nil
	}); err != nil {
		log.WithFunc("localfile.rollbackCreate").Warnf(ctx, "rollback snapshot %s (name=%s): %v", id, name, err)
	}
}

// List returns all snapshots (excluding pending ones).
func (lf *LocalFile) List(ctx context.Context) ([]*types.Snapshot, error) {
	var result []*types.Snapshot
	return result, lf.store.With(ctx, func(idx *snapshot.SnapshotIndex) error {
		for _, rec := range idx.Snapshots {
			if rec == nil || rec.Pending {
				continue
			}
			s := rec.Snapshot // value copy
			result = append(result, &s)
		}
		return nil
	})
}

// Inspect returns a single snapshot by ref (ID, name, or ID prefix).
func (lf *LocalFile) Inspect(ctx context.Context, ref string) (*types.Snapshot, error) {
	var result *types.Snapshot
	return result, lf.store.With(ctx, func(idx *snapshot.SnapshotIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.Snapshots[id]
		if rec == nil || rec.Pending {
			return snapshot.ErrNotFound
		}
		s := rec.Snapshot // value copy
		result = &s
		return nil
	})
}

// Delete removes snapshots by ref. Returns the list of actually deleted IDs.
func (lf *LocalFile) Delete(ctx context.Context, refs []string) ([]string, error) {
	// Resolve all refs under one lock.
	var ids []string
	if err := lf.store.With(ctx, func(idx *snapshot.SnapshotIndex) error {
		var resolveErr error
		ids, resolveErr = idx.ResolveMany(refs)
		return resolveErr
	}); err != nil {
		return nil, err
	}

	// Delete data dirs and DB records.
	var deleted []string
	for _, id := range ids {
		dataDir := lf.conf.SnapshotDataDir(id)
		if err := os.RemoveAll(dataDir); err != nil {
			return deleted, fmt.Errorf("remove data dir %s: %w", id, err)
		}

		if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
			rec := idx.Snapshots[id]
			if rec == nil {
				return nil
			}
			if rec.Name != "" {
				delete(idx.Names, rec.Name)
			}
			delete(idx.Snapshots, id)
			return nil
		}); err != nil {
			return deleted, fmt.Errorf("delete DB record %s: %w", id, err)
		}
		deleted = append(deleted, id)
	}
	return deleted, nil
}

// Restore returns the snapshot config and a streaming tar reader of the snapshot data.
func (lf *LocalFile) Restore(ctx context.Context, ref string) (*types.SnapshotConfig, io.ReadCloser, error) {
	var (
		cfg     *types.SnapshotConfig
		dataDir string
	)
	if err := lf.store.With(ctx, func(idx *snapshot.SnapshotIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.Snapshots[id]
		if rec == nil || rec.Pending {
			return snapshot.ErrNotFound
		}
		cfg = snapshotRecordToConfig(rec)
		dataDir = rec.DataDir
		return nil
	}); err != nil {
		return nil, nil, err
	}

	return cfg, utils.TarDirStream(dataDir, nil), nil
}

// RegisterGC registers the snapshot GC module with the orchestrator.
func (lf *LocalFile) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, gcModule(lf.conf, lf.store, lf.locker))
}

// snapshotRecordToConfig builds a detached SnapshotConfig from a record,
// deep-copying ImageBlobIDs so the caller can use it after the lock is released.
func snapshotRecordToConfig(rec *snapshot.SnapshotRecord) *types.SnapshotConfig {
	blobIDs := make(map[string]struct{}, len(rec.ImageBlobIDs))
	maps.Copy(blobIDs, rec.ImageBlobIDs)
	return &types.SnapshotConfig{
		ID:           rec.ID,
		Name:         rec.Name,
		Description:  rec.Description,
		Image:        rec.Image,
		ImageBlobIDs: blobIDs,
		Hypervisor:   rec.Hypervisor,
		CPU:          rec.CPU,
		Memory:       rec.Memory,
		Storage:      rec.Storage,
		NICs:         rec.NICs,
		Network:      rec.Network,
		Windows:      rec.Windows,
	}
}
