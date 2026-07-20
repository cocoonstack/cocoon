package images

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	gofrsflock "github.com/gofrs/flock"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/utils"
)

// ImageGCSnapshot is the unified GC snapshot for image backends.
type ImageGCSnapshot struct {
	refs    map[string]struct{} // digest hexes referenced by the index
	diskIDs []string            // digest hexes found on disk (blobs + optional extras)
}

// GCModuleConfig configures a generic image GC module.
type GCModuleConfig[E any] struct {
	Name  string
	Store *Store[E]
	// LockPath returns the per-digest blob lock path (design §5).
	LockPath func(hex string) string
	// ReadRefs extracts referenced digest hexes from the index.
	ReadRefs func(map[string]*E) map[string]struct{}
	// ScanDisk returns digest hexes found on disk (blobs).
	ScanDisk func() ([]string, error)
	// ExtraDisk returns additional hex IDs on disk (e.g., OCI boot dirs). Optional.
	ExtraDisk func() ([]string, error)
	// Removers are called per hex ID during collect.
	Removers []func(string) error
	// TempDir for stale temp cleanup.
	TempDir string
	// DirOnly: true for OCI (temp dirs), false for cloudimg (temp files).
	DirOnly bool
}

// BuildGCModule constructs a gc.Module from the config.
func BuildGCModule[E any](cfg GCModuleConfig[E]) gc.Module[ImageGCSnapshot] {
	return gc.Module[ImageGCSnapshot]{
		Name: cfg.Name,
		ReadDB: func(ctx context.Context) (ImageGCSnapshot, error) {
			var snap ImageGCSnapshot
			if err := cfg.Store.View(ctx, func(idx *Index[E]) error {
				snap.refs = cfg.ReadRefs(idx.Images)
				return nil
			}); err != nil {
				return snap, err
			}
			var err error
			if snap.diskIDs, err = cfg.ScanDisk(); err != nil {
				return snap, err
			}
			if cfg.ExtraDisk != nil {
				extra, err := cfg.ExtraDisk()
				if err != nil {
					return snap, err
				}
				snap.diskIDs = append(snap.diskIDs, extra...)
			}
			return snap, nil
		},
		Resolve: func(_ context.Context, snap ImageGCSnapshot, others map[string]any) []string {
			used := gc.Collect(others, gc.BlobIDs)
			allRefs := utils.MergeSets(snap.refs, used)
			candidates := utils.FilterUnreferenced(snap.diskIDs, allRefs)
			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		Collect: func(ctx context.Context, ids []string, _ ImageGCSnapshot) error {
			errs := gcStaleTemp(ctx, cfg.TempDir, cfg.DirOnly)
			// Per-digest lock (TryLock): a publish holding it is mid
			// rename-to-index and must not lose its blob; an unreferenced
			// blob is otherwise recordless — its removal converges without a
			// tombstone. The lock file is kept: unlinking a lock path splits
			// exclusion for a live waiter.
			logger := log.WithFunc("gc." + cfg.Name)
			for _, hex := range ids {
				fl := gofrsflock.New(cfg.LockPath(hex))
				ok, err := fl.TryLock()
				if err != nil || !ok {
					_ = fl.Close()
					if err != nil {
						errs = append(errs, fmt.Errorf("lock blob %s: %w", hex, err))
					} else {
						logger.Warnf(ctx, "skip %s: blob lock busy (publish in flight)", hex)
					}
					continue
				}
				for _, rm := range cfg.Removers {
					if err := rm(hex); err != nil && !errors.Is(err, fs.ErrNotExist) {
						errs = append(errs, fmt.Errorf("remove blob %s: %w", hex, err))
					}
				}
				_ = fl.Close()
				logger.Infof(ctx, "collected blob=%s", hex)
			}
			return errors.Join(errs...)
		},
	}
}

// gcStaleTemp removes temp entries older than StaleTempAge (dirOnly=true skips files); .lock files stay — flock syncs on inode, so deleting one races a current holder.
func gcStaleTemp(ctx context.Context, dir string, dirOnly bool) []error {
	cutoff := time.Now().Add(-utils.StaleTempAge)
	return utils.RemoveMatching(ctx, dir, func(e os.DirEntry) bool {
		if dirOnly && !e.IsDir() {
			return false
		}
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lock") {
			return false
		}
		info, err := e.Info()
		return err == nil && info.ModTime().Before(cutoff)
	})
}
