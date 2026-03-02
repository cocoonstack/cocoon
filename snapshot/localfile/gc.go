package localfile

import (
	"context"
	"errors"
	"os"

	"github.com/projecteru2/cocoon/gc"
	"github.com/projecteru2/cocoon/lock"
	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/storage"
	"github.com/projecteru2/cocoon/utils"
)

// snapshotGCSnapshot is the typed GC snapshot for the snapshot module.
type snapshotGCSnapshot struct {
	blobIDs     map[string]struct{} // union of all snapshots' ImageBlobIDs
	snapshotIDs map[string]struct{} // all snapshot IDs in the DB
	dataDirs    []string            // subdirectory names under DataDir
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
			if err := store.Read(func(idx *snapshot.SnapshotIndex) error {
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
			return utils.FilterUnreferenced(snap.dataDirs, snap.snapshotIDs)
		},
		Collect: func(_ context.Context, ids []string) error {
			var errs []error
			for _, id := range ids {
				if err := os.RemoveAll(conf.SnapshotDataDir(id)); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	}
}
