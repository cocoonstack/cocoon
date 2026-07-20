package localfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	gofrsflock "github.com/gofrs/flock"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/snapshot"
)

// ErrTombstoned reports an entrypoint that met a deleting-phase snapshot.
var ErrTombstoned = errors.New("snapshot is being deleted")

// snapCleanup is the snapshots-namespace tombstone payload.
type snapCleanup struct {
	Name    string `json:"name,omitempty"`
	DataDir string `json:"data_dir,omitempty"`
}

func (lf *LocalFile) tombstones() *tombstone.Table {
	return tombstone.NewTable(lf.meta, metaNS)
}

// deleteSnapshotProtocol runs the §5 phase protocol for one snapshot under
// its held EXCLUSIVE lease. revalidate (optional) re-checks candidacy inside
// the lease transaction; returning false skips without error.
func (lf *LocalFile) deleteSnapshotProtocol(ctx context.Context, id string, revalidate func(*snapshot.SnapshotRecord) bool) (deleted bool, hyp string, err error) {
	ts := lf.tombstones()
	var (
		leaseID string
		cl      snapCleanup
		skip    bool
	)
	if err := lf.update(ctx, func(t *snapTx) error {
		leaseID, cl, skip = "", snapCleanup{}, false
		rec, err := t.Get(id)
		if err != nil {
			return err
		}
		existing, err := ts.Get(ctx, t.Writer(), id)
		if err != nil {
			return err
		}
		if existing == nil {
			if rec == nil || (revalidate != nil && !revalidate(rec)) {
				skip = true
				return nil
			}
			hyp = rec.Hypervisor
			cl = snapCleanup{Name: rec.Name, DataDir: rec.DataDir}
			if cl.DataDir == "" {
				cl.DataDir = lf.conf.SnapshotDataDir(id)
			}
		}
		var resumed *tombstone.Record
		leaseID, resumed, err = ts.Acquire(ctx, t.Writer(), id, func() (tombstone.Payload, error) {
			cleanup, mErr := tombstone.MarshalCleanup(cl)
			if mErr != nil {
				return tombstone.Payload{}, mErr
			}
			return tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup}, nil
		})
		if err != nil || resumed == nil {
			return err
		}
		return json.Unmarshal(resumed.Payload.Cleanup, &cl)
	}); err != nil {
		return false, "", err
	}
	if skip {
		return false, "", nil
	}
	if err := lf.update(ctx, func(t *snapTx) error {
		return ts.MarkDeleting(ctx, t.Writer(), id, leaseID)
	}); err != nil {
		return false, "", err
	}
	if err := lf.finishSnapTeardown(ctx, id, leaseID, cl); err != nil {
		return false, "", err
	}
	return true, hyp, nil
}

func (lf *LocalFile) finishSnapTeardown(ctx context.Context, id, leaseID string, cl snapCleanup) error {
	if err := os.RemoveAll(cl.DataDir); err != nil {
		return fmt.Errorf("remove data dir %s (tombstone kept, retry or gc resumes): %w", id, err)
	}
	ts := lf.tombstones()
	err := lf.update(ctx, func(t *snapTx) error {
		if err := t.Del(id); err != nil {
			return err
		}
		if cl.Name != "" {
			if cur, ok, err := t.NameGet(cl.Name); err != nil {
				return err
			} else if ok && cur == id {
				if err := t.NameDel(cl.Name); err != nil {
					return err
				}
			}
		}
		return ts.Finalize(ctx, t.Writer(), id, leaseID)
	})
	if errors.Is(err, tombstone.ErrLost) {
		return nil
	}
	return err
}

// recoverSnapTombstone drives id's tombstone under a freshly acquired
// exclusive lease: leased rolls back, deleting rolls forward.
func (lf *LocalFile) recoverSnapTombstone(ctx context.Context, id string) error {
	fl := gofrsflock.New(lf.conf.LeasePath(id))
	locked, err := fl.TryLock()
	if err != nil {
		return fmt.Errorf("lease snapshot %s: %w", id, err)
	}
	if !locked {
		return nil // an active holder will meet the tombstone at its own entrypoint
	}
	defer fl.Close() //nolint:errcheck
	return lf.recoverSnapTombstoneLocked(ctx, id)
}

// recoverSnapTombstoneLocked is recovery with the exclusive lease already held.
func (lf *LocalFile) recoverSnapTombstoneLocked(ctx context.Context, id string) error {
	ts := lf.tombstones()
	var (
		rec     *tombstone.Record
		leaseID string
	)
	if err := lf.update(ctx, func(t *snapTx) error {
		var err error
		rec, leaseID, err = ts.Resume(ctx, t.Writer(), id)
		return err
	}); err != nil {
		return err
	}
	if rec == nil || rec.Phase == tombstone.PhaseLeased {
		return nil
	}
	var cl snapCleanup
	if err := json.Unmarshal(rec.Payload.Cleanup, &cl); err != nil {
		return fmt.Errorf("tombstone %s payload: %w", id, err)
	}
	if err := lf.finishSnapTeardown(ctx, id, leaseID, cl); err != nil {
		return err
	}
	log.WithFunc("localfile.recoverSnapTombstone").Warnf(ctx, "rolled forward interrupted delete of snapshot %s", id)
	return nil
}

// guardSnapTombstone is the §5 shared-lease escalation: release shared,
// recover exclusively, report ErrTombstoned — an in-place upgrade deadlocks.
func (lf *LocalFile) guardSnapTombstone(ctx context.Context, id string, releaseShared func()) error {
	var present bool
	if err := lf.view(ctx, func(t *snapTx) error {
		rec, err := lf.tombstones().Get(ctx, t.Reader(), id)
		present = rec != nil
		return err
	}); err != nil {
		// The guard consumes the lease on every path: leaking a shared flock
		// here would block exclusive delete/GC for the process lifetime.
		releaseShared()
		return err
	}
	if !present {
		return nil
	}
	releaseShared()
	if err := lf.recoverSnapTombstone(ctx, id); err != nil {
		return fmt.Errorf("snapshot %s: recover interrupted delete: %w", id, err)
	}
	return fmt.Errorf("snapshot %s: %w", id, ErrTombstoned)
}
