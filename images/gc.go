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

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/utils"
)

// ImageGCSnapshot is the unified GC snapshot for image backends.
type ImageGCSnapshot struct {
	refs    map[string]struct{} // digest hexes referenced by the index
	diskIDs []string            // digest hexes found on disk (blobs + optional extras)
}

// GCModuleConfig configures a generic image GC module.
type GCModuleConfig[E any] struct {
	Name   string
	Locker lock.Locker
	Store  *Store[E]
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
		Name:   cfg.Name,
		Locker: cfg.Locker,
		ReadDB: func(ctx context.Context) (ImageGCSnapshot, error) {
			var snap ImageGCSnapshot
			// Lockless read: the orchestrator already holds the namespace lock.
			if err := cfg.Store.LockedRead(ctx, func(idx *Index[E]) error {
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
			return gcCollectBlobs(ctx, cfg.Name, cfg.TempDir, cfg.DirOnly, ids, cfg.Removers...)
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

// gcCollectBlobs removes stale temps then per-hex blobs via removers (fs.ErrNotExist ignored); module names the gc log subsystem.
func gcCollectBlobs(ctx context.Context, module, tempDir string, dirOnly bool, ids []string, removers ...func(string) error) error {
	logger := log.WithFunc("gc." + module)
	var errs []error
	errs = append(errs, gcStaleTemp(ctx, tempDir, dirOnly)...)
	for _, hex := range ids {
		var blobErr error
		for _, rm := range removers {
			if err := rm(hex); err != nil && !errors.Is(err, fs.ErrNotExist) {
				blobErr = errors.Join(blobErr, err)
			}
		}
		if blobErr != nil {
			errs = append(errs, fmt.Errorf("remove blob %s: %w", hex, blobErr))
			continue
		}
		logger.Infof(ctx, "collected blob=%s reason=unreferenced", hex)
	}
	return errors.Join(errs...)
}
