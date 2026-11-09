package localfile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	reasonOrphan       = "orphan"
	reasonStalePending = "stale-pending"
	reasonMissingDir   = "missing-dir"
)

// EvictionPolicy controls LRU snapshot eviction; Enabled with zero criteria evicts all non-pending.
type EvictionPolicy struct {
	Enabled  bool
	DryRun   bool
	KeepLast int
	MaxAge   time.Duration
	MaxSize  int64
}

func (p EvictionPolicy) hasCriteria() bool {
	return p.KeepLast > 0 || p.MaxAge > 0 || p.MaxSize > 0
}

type snapshotMeta struct {
	name         string
	lastAccessed time.Time
	sizeBytes    int64
}

type snapshotGCSnapshot struct {
	blobIDs      map[string]struct{}
	snapshotIDs  map[string]struct{}
	dataDirs     []string
	leaseIDs     []string
	stalePending []string
	missingDir   []string
	records      map[string]snapshotMeta
	reasons      map[string]string
}

func (s snapshotGCSnapshot) UsedBlobIDs() map[string]struct{} { return s.blobIDs }

// PinnedBlobIDs reports every image blob pinned by a snapshot record — image GC's under-digest-lock recheck source.
func (lf *LocalFile) PinnedBlobIDs(ctx context.Context) (map[string]struct{}, error) {
	pins := map[string]struct{}{}
	if err := lf.view(ctx, func(t *snapTx) error {
		return t.Scan(func(_ string, rec *snapshotRecord) error {
			maps.Copy(pins, rec.ImageBlobIDs)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	return pins, nil
}

func (lf *LocalFile) gcRecover(ctx context.Context) []error {
	var ids []string
	if err := lf.view(ctx, func(t *snapTx) error {
		var err error
		ids, err = lf.tombstones().PendingIDs(ctx, t.Reader())
		return err
	}); err != nil {
		return []error{err}
	}
	var errs []error
	for _, id := range ids {
		if err := lf.recoverSnapTombstone(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func gcModule(lf *LocalFile, policy EvictionPolicy) gc.Module[snapshotGCSnapshot] {
	conf, recorder := lf.conf, lf.metering
	return gc.Module[snapshotGCSnapshot]{
		Name:    "snapshot",
		Recover: lf.gcRecover,
		ReadDB: func(ctx context.Context) (snapshotGCSnapshot, error) {
			snap := snapshotGCSnapshot{reasons: make(map[string]string)}
			if err := lf.view(ctx, func(t *snapTx) error {
				snap.blobIDs = make(map[string]struct{})
				snap.snapshotIDs = make(map[string]struct{})
				snap.records = make(map[string]snapshotMeta)
				snap.stalePending = nil
				return t.Scan(func(id string, rec *snapshotRecord) error {
					snap.snapshotIDs[id] = struct{}{}
					maps.Copy(snap.blobIDs, rec.ImageBlobIDs)
					if rec.Pending {
						snap.stalePending = append(snap.stalePending, id)
						return nil
					}
					snap.records[id] = snapshotMeta{
						name:         rec.Name,
						lastAccessed: cmp.Or(rec.LastAccessedAt, rec.CreatedAt),
						sizeBytes:    rec.SizeBytes,
					}
					return nil
				})
			}); err != nil {
				return snap, err
			}
			// The stats run after the read transaction closes: a large store would otherwise pin WAL frames for the whole walk
			for id := range snap.records {
				if _, statErr := os.Stat(conf.SnapshotDataDir(id)); errors.Is(statErr, fs.ErrNotExist) {
					snap.missingDir = append(snap.missingDir, id)
				}
			}
			var err error
			if snap.dataDirs, err = utils.ScanSubdirs(conf.DataDir()); err != nil {
				return snap, err
			}
			if snap.leaseIDs, err = utils.ScanFileStems(conf.DataDir(), leaseSuffix); err != nil {
				return snap, err
			}
			return snap, nil
		},
		Resolve: func(ctx context.Context, snap snapshotGCSnapshot, _ map[string]any) []string {
			orphans := utils.FilterUnreferenced(snap.dataDirs, snap.snapshotIDs)
			for _, id := range orphans {
				snap.reasons[id] = reasonOrphan
			}
			for _, id := range snap.stalePending {
				snap.reasons[id] = reasonStalePending
			}
			for _, id := range snap.missingDir {
				snap.reasons[id] = reasonMissingDir
			}
			candidates := slices.Concat(orphans, snap.stalePending, snap.missingDir)

			if policy.Enabled {
				lruReasons := pickLRU(snap.records, policy)
				if policy.DryRun {
					logWouldEvict(ctx, lruReasons, snap.records)
				} else {
					maps.Copy(snap.reasons, lruReasons)
					candidates = slices.AppendSeq(candidates, maps.Keys(lruReasons))
				}
			}

			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		Collect: func(ctx context.Context, ids []string, snap snapshotGCSnapshot) error {
			logger := log.WithFunc("gc.snapshot")
			var errs []error
			for _, id := range ids {
				if err := ctx.Err(); err != nil {
					errs = append(errs, err)
					break
				}
				// An exclusive acquire proves the pending record's owner died; no age gate needed
				release, ok, lockErr := lf.tryExclusiveLease(ctx, id)
				if lockErr != nil {
					errs = append(errs, lockErr)
					continue
				}
				if !ok {
					logger.Warnf(ctx, "skip %s: leased by an active holder", id)
					continue
				}
				// The reason picked at ReadDB must still hold, or a create in the window evicts the wrong snapshot
				var sawRecord bool
				revalidate := func(rec *snapshotRecord) bool {
					sawRecord = true
					switch snap.reasons[id] {
					case reasonStalePending:
						return rec.Pending
					case reasonMissingDir:
						_, statErr := os.Stat(conf.SnapshotDataDir(id))
						return errors.Is(statErr, fs.ErrNotExist)
					case reasonOrphan:
						return false // a record appeared for a dir orphaned at ReadDB
					default: // LRU picks: a touch since ReadDB voids the eviction choice
						m, ok := snap.records[id]
						return ok && cmp.Or(rec.LastAccessedAt, rec.CreatedAt).Equal(m.lastAccessed)
					}
				}
				deleted, cleanup, err := lf.deleteSnapshotProtocol(ctx, id, revalidate)
				if err != nil {
					release()
					errs = append(errs, fmt.Errorf("collect snapshot %s: %w", id, err))
					continue
				}
				if !deleted {
					if snap.reasons[id] != reasonOrphan || sawRecord {
						release() // candidacy voided under the lease; the next cycle re-picks
						continue
					}
					if err := os.RemoveAll(conf.SnapshotDataDir(id)); err != nil {
						release()
						errs = append(errs, fmt.Errorf("remove snapshot %s: %w", id, err))
						continue
					}
				}
				release()
				logEvictRow(ctx, logger, "collected", id, snap.records[id], snap.reasons[id])
				if deleted && !cleanup.Pending {
					emitSnapStop(ctx, recorder, id, cleanup.Hypervisor)
				}
			}
			sweepLeases(ctx, conf, snap)
			return errors.Join(errs...)
		},
	}
}

// sweepLeases reclaims unheld lease files of ids with no record.
func sweepLeases(ctx context.Context, conf *Config, snap snapshotGCSnapshot) {
	logger := log.WithFunc("gc.snapshot")
	for _, id := range utils.FilterUnreferenced(snap.leaseIDs, snap.snapshotIDs) {
		ok, err := flock.ReclaimTransient(ctx, conf.LeasePath(id))
		if err != nil {
			logger.Warnf(ctx, "sweep lease %s: %v", id, err)
			continue
		}
		if ok {
			logger.Infof(ctx, "collected id=%s reason=orphan-lease", id)
		}
	}
}

func pickLRU(records map[string]snapshotMeta, p EvictionPolicy) map[string]string {
	sorted := slices.SortedFunc(maps.Keys(records), func(a, b string) int {
		return records[a].lastAccessed.Compare(records[b].lastAccessed)
	})

	reasons := make(map[string]string)

	if !p.hasCriteria() {
		for _, id := range sorted {
			reasons[id] = "lru-all"
		}
		return reasons
	}

	add := func(id, label string) {
		if existing, ok := reasons[id]; ok {
			reasons[id] = existing + "+" + label
			return
		}
		reasons[id] = label
	}

	if p.MaxAge > 0 {
		cutoff := time.Now().Add(-p.MaxAge)
		for _, id := range sorted {
			if records[id].lastAccessed.Before(cutoff) {
				add(id, "lru-age")
			}
		}
	}

	if p.KeepLast > 0 && len(sorted) > p.KeepLast {
		for _, id := range sorted[:len(sorted)-p.KeepLast] {
			add(id, "lru-keep")
		}
	}

	if p.MaxSize > 0 {
		var total int64
		for _, id := range sorted {
			total += records[id].sizeBytes
		}
		for _, id := range sorted {
			if total <= p.MaxSize {
				break
			}
			add(id, "lru-size")
			total -= records[id].sizeBytes
		}
	}

	return reasons
}

func logWouldEvict(ctx context.Context, reasons map[string]string, records map[string]snapshotMeta) {
	logger := log.WithFunc("gc.snapshot")
	for _, id := range slices.Sorted(maps.Keys(reasons)) {
		logEvictRow(ctx, logger, "would-evict", id, records[id], reasons[id])
	}
}

func logEvictRow(ctx context.Context, logger *log.Fields, verb, id string, m snapshotMeta, reason string) {
	accessed := "never"
	if !m.lastAccessed.IsZero() {
		accessed = m.lastAccessed.UTC().Format(time.RFC3339)
	}
	logger.Infof(ctx, "%s id=%s name=%s bytes=%d last_accessed=%s reason=%s",
		verb, id, m.name, m.sizeBytes, accessed, reason)
}
