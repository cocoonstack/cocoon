package localfile

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

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

// compile-time interface check.
var _ snapshot.Snapshot = (*LocalFile)(nil)

// LocalFile implements snapshot.Snapshot using the local filesystem.
type LocalFile struct {
	conf   *snapshot.Config
	store  storage.Store[snapshot.SnapshotIndex]
	locker lock.Locker
}

// New creates a new LocalFile snapshot backend.
func New(conf *config.Config) (*LocalFile, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := snapshot.NewConfig(conf)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	locker := flock.New(cfg.IndexLock())
	store := storejson.New[snapshot.SnapshotIndex](cfg.IndexFile(), locker)
	return &LocalFile{conf: cfg, store: store, locker: locker}, nil
}

// Create persists a snapshot from the given config and tar.gz data stream.
// The stream is extracted into a per-snapshot data directory, and a DB record is written.
func (lf *LocalFile) Create(ctx context.Context, cfg *types.SnapshotConfig, stream io.Reader) (string, error) {
	id, err := utils.GenerateID()
	if err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}

	dataDir := lf.conf.SnapshotDataDir(id)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}

	// Extract tar.gz stream into the data directory.
	if err := utils.ExtractTarGz(dataDir, stream); err != nil {
		os.RemoveAll(dataDir) //nolint:errcheck,gosec
		return "", fmt.Errorf("extract snapshot data: %w", err)
	}

	// Persist the DB record.
	now := time.Now()
	if err := lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		// Check name uniqueness.
		if cfg.Name != "" {
			if existingID, ok := idx.Names[cfg.Name]; ok {
				return fmt.Errorf("snapshot name %q already in use by %s", cfg.Name, existingID)
			}
		}

		idx.Snapshots[id] = &snapshot.SnapshotRecord{
			Snapshot: types.Snapshot{
				ID:          id,
				Name:        cfg.Name,
				Description: cfg.Description,
				CreatedAt:   now,
			},
			ImageBlobIDs: cfg.ImageBlobIDs,
			DataDir:      dataDir,
		}
		if cfg.Name != "" {
			idx.Names[cfg.Name] = id
		}
		return nil
	}); err != nil {
		os.RemoveAll(dataDir) //nolint:errcheck,gosec
		return "", err
	}

	return id, nil
}

// List returns all snapshots.
func (lf *LocalFile) List(ctx context.Context) ([]*types.Snapshot, error) {
	var result []*types.Snapshot
	return result, lf.store.With(ctx, func(idx *snapshot.SnapshotIndex) error {
		for _, rec := range idx.Snapshots {
			if rec == nil {
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
		if rec == nil {
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
		seen := make(map[string]struct{}, len(refs))
		for _, ref := range refs {
			id, err := snapshot.ResolveSnapshotRef(idx, ref)
			if err != nil {
				return fmt.Errorf("resolve %q: %w", ref, err)
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		return nil
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

// RegisterGC registers the snapshot GC module with the orchestrator.
func (lf *LocalFile) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, snapshot.GCModule(lf.conf, lf.store, lf.locker))
}
