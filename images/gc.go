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

	gofrsflock "github.com/gofrs/flock"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/utils"
)

// PinRecheck reports blobs pinned by VM/snapshot records, consulted under the digest lock since the candidate list predates those pins.
type PinRecheck func(ctx context.Context) (map[string]struct{}, error)

// ImageGCSnapshot is the unified GC snapshot for image backends.
type ImageGCSnapshot struct {
	refs    map[string]struct{} // digest hexes referenced by the index
	diskIDs []string            // digest hexes found on disk (blobs + optional extras)
}

// GCModuleConfig configures a generic image GC module.
type GCModuleConfig[E any] struct {
	Name     string
	Store    *Store[E]
	LockPath func(hex string) string
	ReadRefs func(map[string]*E) map[string]struct{}
	ScanDisk func() ([]string, error)
	// ExtraDisk returns additional hex IDs on disk (e.g., OCI boot dirs). Optional.
	ExtraDisk func() ([]string, error)
	Removers  []func(string) error
	TempDir   string
	// DirOnly: true for OCI (temp dirs), false for cloudimg (temp files).
	DirOnly bool
	// PinnedElsewhere is optional.
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
			errs := gcStaleTemp(ctx, cfg.TempDir, cfg.DirOnly)
			// TryLock: a publish holding it is mid rename-to-index and must not lose its blob; the lock file is kept since unlinking it would split exclusion for a live waiter.
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
				// Revalidate under the held lock: a publish that finished after the snapshot may have referenced this digest.
				referenced := false
				if err := cfg.Store.View(ctx, func(idx *Index[E]) error {
					_, referenced = cfg.ReadRefs(idx.Images)[hex]
					return nil
				}); err != nil {
					_ = fl.Close()
					errs = append(errs, fmt.Errorf("revalidate blob %s: %w", hex, err))
					continue
				}
				if !referenced && cfg.PinnedElsewhere != nil {
					pins, err := cfg.PinnedElsewhere(ctx)
					if err != nil {
						_ = fl.Close()
						errs = append(errs, fmt.Errorf("recheck pins for blob %s: %w", hex, err))
						continue
					}
					_, referenced = pins[hex]
				}
				if referenced {
					_ = fl.Close()
					continue
				}
				for _, rm := range cfg.Removers {
					if err := rm(hex); err != nil && !errors.Is(err, fs.ErrNotExist) {
						errs = append(errs, fmt.Errorf("remove blob %s: %w", hex, err))
					}
				}
				_ = fl.Close()
				logger.Infof(ctx, "collected blob=%s reason=unreferenced", hex)
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
