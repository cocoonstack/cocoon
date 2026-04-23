package localfile

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

// pendingGCGrace is the age after which a pending (incomplete) snapshot
// record is considered stale and eligible for GC cleanup. 24h accommodates
// large snapshots on slow storage; snapshots typically complete in under a minute.
const pendingGCGrace = 24 * time.Hour

// snapshotGCSnapshot is the typed GC snapshot for the snapshot module.
type snapshotGCSnapshot struct {
	blobIDs      map[string]struct{} // union of all snapshots' ImageBlobIDs
	snapshotIDs  map[string]struct{} // all snapshot IDs in the DB
	dataDirs     []string            // subdirectory names under DataDir
	stalePending []string            // IDs in stale "pending" state (crash remnants)
}

// UsedBlobIDs implements the gc.usedBlobIDs protocol.
func (s snapshotGCSnapshot) UsedBlobIDs() map[string]struct{} { return s.blobIDs }

// gcModule returns the GC module for the localfile snapshot backend.
func gcModule(conf *Config, store storage.Store[snapshot.SnapshotIndex], locker lock.Locker) gc.Module[snapshotGCSnapshot] {
	return gc.Module[snapshotGCSnapshot]{
		Name:   "snapshot",
		Locker: locker,
		ReadDB: func(_ context.Context) (snapshotGCSnapshot, error) {
			var snap snapshotGCSnapshot
			cutoff := time.Now().Add(-pendingGCGrace)
			if err := store.ReadRaw(func(idx *snapshot.SnapshotIndex) error {
				snap.blobIDs = make(map[string]struct{})
				snap.snapshotIDs = make(map[string]struct{})
				for id, rec := range idx.Snapshots {
					if rec == nil {
						continue
					}
					snap.snapshotIDs[id] = struct{}{}
					for hex := range rec.ImageBlobIDs {
						snap.blobIDs[hex] = struct{}{}
					}
					if rec.Pending && rec.CreatedAt.Before(cutoff) {
						snap.stalePending = append(snap.stalePending, id)
					}
				}
				return nil
			}); err != nil {
				return snap, err
			}
			var err error
			if snap.dataDirs, err = utils.ScanSubdirs(conf.DataDir()); err != nil {
				return snap, err
			}
			return snap, nil
		},
		Resolve: func(snap snapshotGCSnapshot, _ map[string]any) []string {
			orphans := utils.FilterUnreferenced(snap.dataDirs, snap.snapshotIDs)
			candidates := slices.Concat(orphans, snap.stalePending)
			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		Collect: func(ctx context.Context, ids []string) error {
			var errs []error
			for _, id := range ids {
				if err := os.RemoveAll(conf.SnapshotDataDir(id)); err != nil {
					errs = append(errs, err)
				}
			}
			if err := cleanStalePending(store, ids); err != nil {
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		},
	}
}

// cleanStalePending removes selected DB records stuck in stale "pending"
// state. IDs not found (or no longer pending) are skipped.
func cleanStalePending(store storage.Store[snapshot.SnapshotIndex], ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-pendingGCGrace)
	return store.WriteRaw(func(idx *snapshot.SnapshotIndex) error {
		utils.CleanStaleRecords(idx.Snapshots, idx.Names, ids,
			func(r *snapshot.SnapshotRecord) string { return r.Name },
			func(r *snapshot.SnapshotRecord) bool {
				return r.Pending && r.CreatedAt.Before(cutoff)
			},
		)
		return nil
	})
}
