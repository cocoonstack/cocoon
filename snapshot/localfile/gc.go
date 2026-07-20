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

	gofrsflock "github.com/gofrs/flock"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/utils"
)

// pendingGCGrace lets a slow-storage snapshot finish before GC reclaims a pending record.
const pendingGCGrace = 24 * time.Hour

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
	hypervisor   string
	lastAccessed time.Time
	sizeBytes    int64
}

type snapshotGCSnapshot struct {
	blobIDs      map[string]struct{}
	snapshotIDs  map[string]struct{}
	dataDirs     []string
	stalePending []string
	missingDir   []string // finalized records whose data dir vanished (rm dir-deleted, DB update failed)
	records      map[string]snapshotMeta
	reasons      map[string]string
	policy       EvictionPolicy
}

func (s snapshotGCSnapshot) UsedBlobIDs() map[string]struct{} { return s.blobIDs }

func gcModule(lf *LocalFile, policy EvictionPolicy) gc.Module[snapshotGCSnapshot] {
	conf, locker, recorder := lf.conf, lf.locker, lf.metering
	return gc.Module[snapshotGCSnapshot]{
		Name:   "snapshot",
		Locker: locker,
		ReadDB: func(ctx context.Context) (snapshotGCSnapshot, error) {
			snap := snapshotGCSnapshot{policy: policy, reasons: make(map[string]string)}
			cutoff := time.Now().Add(-pendingGCGrace)
			// Lockless read: the orchestrator already holds the namespace lock.
			if err := lf.rawView(ctx, func(t *snapTx) error {
				snap.blobIDs = make(map[string]struct{})
				snap.snapshotIDs = make(map[string]struct{})
				snap.records = make(map[string]snapshotMeta)
				return t.Scan(func(id string, rec *snapshot.SnapshotRecord) error {
					snap.snapshotIDs[id] = struct{}{}
					maps.Copy(snap.blobIDs, rec.ImageBlobIDs)
					if rec.Pending {
						if rec.CreatedAt.Before(cutoff) {
							snap.stalePending = append(snap.stalePending, id)
						}
						return nil
					}
					if _, statErr := os.Stat(cmp.Or(rec.DataDir, conf.SnapshotDataDir(id))); errors.Is(statErr, fs.ErrNotExist) {
						snap.missingDir = append(snap.missingDir, id)
					}
					snap.records[id] = snapshotMeta{
						name:         rec.Name,
						hypervisor:   rec.Hypervisor,
						lastAccessed: rec.LastAccessedAt,
						sizeBytes:    rec.SizeBytes,
					}
					return nil
				})
			}); err != nil {
				return snap, err
			}
			var err error
			if snap.dataDirs, err = utils.ScanSubdirs(conf.DataDir()); err != nil {
				return snap, err
			}
			if policy.MaxSize > 0 {
				backfillSizeBytes(ctx, lf, snap.records)
			}
			return snap, nil
		},
		Resolve: func(ctx context.Context, snap snapshotGCSnapshot, _ map[string]any) []string {
			orphans := utils.FilterUnreferenced(snap.dataDirs, snap.snapshotIDs)
			for _, id := range orphans {
				snap.reasons[id] = "orphan"
			}
			for _, id := range snap.stalePending {
				snap.reasons[id] = "stale-pending"
			}
			for _, id := range snap.missingDir {
				snap.reasons[id] = "missing-dir"
			}
			candidates := slices.Concat(orphans, snap.stalePending, snap.missingDir)

			if snap.policy.Enabled {
				lruReasons := pickLRU(snap.records, snap.policy)
				if snap.policy.DryRun {
					logWouldEvict(ctx, lruReasons, snap.records)
				} else {
					maps.Copy(snap.reasons, lruReasons)
					candidates = append(candidates, slices.Collect(maps.Keys(lruReasons))...)
				}
			}

			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		Collect: func(ctx context.Context, ids []string, snap snapshotGCSnapshot) error {
			logger := log.WithFunc("gc.snapshot")
			var (
				errs    []error
				removed = make([]string, 0, len(ids))
				emits   = make([]string, 0, len(ids))
			)
			for _, id := range ids {
				if err := ctx.Err(); err != nil {
					errs = append(errs, err)
					break
				}
				// Exclusive lease: an active clone/restore/export holds it shared; skip and retry next cycle.
				fl := gofrsflock.New(conf.LeasePath(id))
				locked, lockErr := fl.TryLock()
				if lockErr != nil {
					_ = fl.Close()
					errs = append(errs, fmt.Errorf("lease snapshot %s: %w", id, lockErr))
					continue
				}
				if !locked {
					_ = fl.Close()
					logger.Warnf(ctx, "skip %s: leased by an active reader", id)
					continue
				}
				if err := os.RemoveAll(conf.SnapshotDataDir(id)); err != nil {
					_ = fl.Close()
					errs = append(errs, fmt.Errorf("remove snapshot %s: %w", id, err))
					continue
				}
				_ = fl.Close() // lease file kept: unlinking a lock path splits exclusion (design §5)
				logEvictRow(ctx, logger, "collected", id, snap.records[id], snap.reasons[id])
				removed = append(removed, id)
				// Skip orphan dirs and stale-pending — they never opened a snap.storage interval.
				if _, ok := snap.records[id]; ok {
					emits = append(emits, id)
				}
			}
			// Emit only after the record deletion lands: a persistently failing DB would re-candidate these ids and double-close the interval.
			if err := cleanResolvedRecords(ctx, lf, removed); err != nil {
				errs = append(errs, fmt.Errorf("clean DB records: %w", err))
			} else {
				for _, id := range emits {
					emitSnapStop(ctx, recorder, id, snap.records[id].hypervisor)
				}
			}
			return errors.Join(errs...)
		},
	}
}

// pickLRU maps each evict ID to its reason ("+" joins multi-match; no criteria → "lru-all").
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
	if len(reasons) == 0 {
		return
	}
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

// backfillSizeBytes computes + persists SizeBytes for records missing it so future GC skips du.
func backfillSizeBytes(ctx context.Context, lf *LocalFile, records map[string]snapshotMeta) {
	conf := lf.conf
	logger := log.WithFunc("gc.snapshot")
	var changed bool
	for id, m := range records {
		if m.sizeBytes > 0 {
			continue
		}
		actual, err := utils.DirSize(conf.SnapshotDataDir(id))
		if err != nil {
			logger.Warnf(ctx, "DirSize for %s: %v", id, err)
			continue
		}
		m.sizeBytes = actual
		records[id] = m
		changed = true
	}
	if !changed {
		return
	}
	if err := lf.lockedUpdate(ctx, func(t *snapTx) error {
		for id, m := range records {
			r, err := t.Get(id)
			if err != nil {
				return err
			}
			if r == nil || r.SizeBytes == m.sizeBytes {
				continue
			}
			r.SizeBytes = m.sizeBytes
			if err := t.Put(id, r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		logger.Warnf(ctx, "persist backfilled SizeBytes: %v", err)
	}
}

// cleanResolvedRecords drops resolved records; pending only past grace.
func cleanResolvedRecords(ctx context.Context, lf *LocalFile, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-pendingGCGrace)
	stale := func(r *snapshot.SnapshotRecord) bool {
		if r.Pending {
			return r.CreatedAt.Before(cutoff)
		}
		return true
	}
	return lf.lockedUpdate(ctx, func(t *snapTx) error {
		for _, id := range ids {
			rec, err := t.Get(id)
			if err != nil {
				return err
			}
			if rec == nil || !stale(rec) {
				continue
			}
			if n := rec.Name; n != "" {
				if err := t.NameDel(n); err != nil {
					return err
				}
			}
			if err := t.Del(id); err != nil {
				return err
			}
		}
		return nil
	})
}
