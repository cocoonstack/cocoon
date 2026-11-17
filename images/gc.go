package images

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	gofrsflock "github.com/gofrs/flock"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/utils"
)

// PinRecheck reports blobs pinned by VM/snapshot records, consulted under the digest lock since the candidate list predates those pins.
type PinRecheck func(ctx context.Context) (map[string]struct{}, error)

// ImageGCSnapshot is the unified GC snapshot for image backends.
type ImageGCSnapshot struct {
	refs    map[string]struct{}
	diskIDs []string
}

// DiskScanFunc returns hex IDs present on disk.
type DiskScanFunc func() ([]string, error)

type BlobRemover func(hex string) error

// GCModuleConfig configures a generic image GC module.
type GCModuleConfig[E any] struct {
	Name     string
	Store    *Store[E]
	LockPath func(hex string) string
	ReadRefs func(map[string]*E) map[string]struct{}
	ScanDisk DiskScanFunc
	// ExtraDisk returns additional hex IDs on disk (e.g., OCI boot dirs). Optional.
	ExtraDisk       DiskScanFunc
	Removers        []BlobRemover
	TempDir         string
	PinnedElsewhere PinRecheck
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
			candidates := utils.FilterUnreferenced(snap.diskIDs, snap.refs, used)
			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		Collect: func(ctx context.Context, ids []string, _ ImageGCSnapshot) error {
			return errors.Join(append(gcStaleTemp(ctx, cfg.TempDir), collectBlobs(ctx, cfg, ids)...)...)
		},
	}
}

// gcStaleTemp removes temp entries whose whole tree is older than StaleTempAge.
func gcStaleTemp(ctx context.Context, dir string) []error {
	cutoff := time.Now().Add(-utils.StaleTempAge)
	return utils.RemoveMatching(ctx, dir, func(e os.DirEntry) bool {
		return !touchedSince(filepath.Join(dir, e.Name()), cutoff)
	})
}

func touchedSince(root string, cutoff time.Time) bool {
	touched := false
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		var info fs.FileInfo
		if err == nil {
			info, err = d.Info()
		}
		if err != nil || !info.ModTime().Before(cutoff) {
			touched = true
			return fs.SkipAll
		}
		return nil
	})
	return touched
}

func collectBlobs[E any](ctx context.Context, cfg GCModuleConfig[E], ids []string) []error {
	logger := log.WithFunc("gc." + cfg.Name)
	var errs []error
	var locked []string
	for _, hex := range ids {
		fl := gofrsflock.New(cfg.LockPath(hex))
		defer fl.Close() //nolint:errcheck
		// TryLock: a publish holding it is mid rename-to-index and must not lose its blob.
		ok, err := fl.TryLock()
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("lock blob %s: %w", hex, err))
		case !ok:
			logger.Warnf(ctx, "skip %s: blob lock busy (publish in flight)", hex)
		default:
			locked = append(locked, hex)
		}
	}
	if len(locked) == 0 {
		return errs
	}
	// Revalidate under the held locks: a publish that finished after the snapshot may have referenced these digests.
	var refs map[string]struct{}
	if err := cfg.Store.View(ctx, func(idx *Index[E]) error {
		refs = cfg.ReadRefs(idx.Images)
		return nil
	}); err != nil {
		return append(errs, fmt.Errorf("revalidate blobs: %w", err))
	}
	unreferenced := utils.FilterUnreferenced(locked, refs)
	if len(unreferenced) > 0 && cfg.PinnedElsewhere != nil {
		pins, err := cfg.PinnedElsewhere(ctx)
		if err != nil {
			return append(errs, fmt.Errorf("recheck pins: %w", err))
		}
		unreferenced = utils.FilterUnreferenced(unreferenced, pins)
	}
	for _, hex := range unreferenced {
		n := len(errs)
		for _, rm := range cfg.Removers {
			if err := rm(hex); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove blob %s: %w", hex, err))
			}
		}
		if len(errs) == n {
			logger.Infof(ctx, "collected blob=%s reason=unreferenced", hex)
		}
	}
	return errs
}
