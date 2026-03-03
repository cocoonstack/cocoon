package localfile

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/projecteru2/cocoon/config"
	"github.com/projecteru2/cocoon/gc"
	"github.com/projecteru2/cocoon/lock"
	flock "github.com/projecteru2/cocoon/lock/flock"
	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/storage"
	storejson "github.com/projecteru2/cocoon/storage/json"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

const typ = "localfile"

// compile-time interface check.
var _ snapshot.Snapshot = (*LocalFile)(nil)

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

// Create persists a snapshot from the given config and data stream.
//
// Uses a two-phase pattern (placeholder → extract → finalize) so that
// a crash between phases leaves a pending record that GC will clean up,
// rather than an orphan data directory with no DB entry.
func (lf *LocalFile) Create(ctx context.Context, cfg *types.SnapshotConfig, stream io.Reader) (string, error) {
	id, err := utils.GenerateID()
	if err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}

	dataDir := lf.conf.SnapshotDataDir(id)
	now := time.Now()

	// Phase 1: write placeholder record so GC won't orphan our dir.
	if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		if cfg.Name != "" {
			if existingID, ok := idx.Names[cfg.Name]; ok {
				return fmt.Errorf("snapshot name %q already in use by %s", cfg.Name, existingID)
			}
		}
		idx.Snapshots[id] = &snapshot.SnapshotRecord{
			Snapshot: types.Snapshot{
				SnapshotConfig: *cfg,
				ID:             id,
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
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dataDir) //nolint:errcheck,gosec
			lf.rollbackCreate(ctx, id, cfg.Name)
		}
	}()

	// Phase 2: create dir + extract data.
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	if err := utils.ExtractTar(dataDir, stream); err != nil {
		return "", fmt.Errorf("extract snapshot data: %w", err)
	}

	// Phase 3: finalize — clear pending flag.
	if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		rec := idx.Snapshots[id]
		if rec == nil {
			return fmt.Errorf("snapshot %q disappeared from index", id)
		}
		rec.Pending = false
		return nil
	}); err != nil {
		return "", fmt.Errorf("finalize snapshot: %w", err)
	}

	success = true
	return id, nil
}

// rollbackCreate removes a placeholder snapshot record from the DB.
func (lf *LocalFile) rollbackCreate(ctx context.Context, id, name string) {
	if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		delete(idx.Snapshots, id)
		delete(idx.Names, name)
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
		id, err := snapshot.ResolveSnapshotRef(idx, ref)
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
		ids, resolveErr = utils.ResolveRefs(idx.Snapshots, idx.Names, refs, snapshot.ErrNotFound)
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
		id, err := snapshot.ResolveSnapshotRef(idx, ref)
		if err != nil {
			return err
		}
		rec := idx.Snapshots[id]
		if rec == nil || rec.Pending {
			return snapshot.ErrNotFound
		}
		// Deep copy ImageBlobIDs.
		blobIDs := make(map[string]struct{}, len(rec.ImageBlobIDs))
		maps.Copy(blobIDs, rec.ImageBlobIDs)
		cfg = &types.SnapshotConfig{
			Name:         rec.Name,
			Description:  rec.Description,
			Image:        rec.Image,
			ImageBlobIDs: blobIDs,
		}
		dataDir = rec.DataDir
		return nil
	}); err != nil {
		return nil, nil, err
	}

	// Stream the data directory as a tar archive via io.Pipe.
	pr, pw := io.Pipe()
	go func() {
		var streamErr error
		defer func() {
			if streamErr != nil {
				pw.CloseWithError(streamErr) //nolint:errcheck,gosec
			} else {
				pw.Close() //nolint:errcheck,gosec
			}
		}()

		tw := tar.NewWriter(pw)
		streamErr = utils.TarDir(tw, dataDir)
		if closeErr := tw.Close(); streamErr == nil {
			streamErr = closeErr
		}
	}()

	return cfg, pr, nil
}

// RegisterGC registers the snapshot GC module with the orchestrator.
func (lf *LocalFile) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, gcModule(lf.conf, lf.store, lf.locker))
}
