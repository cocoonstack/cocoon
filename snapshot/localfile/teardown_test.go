package localfile

import (
	"errors"
	"os"
	"testing"

	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/snapshot"
)

func TestSharedLeaseEscalationLeasedRollsBack(t *testing.T) {
	lf := newTestLF(t)
	id := makeExportableSnapshot(t, lf, "esc-leased", map[string][]byte{"disk.raw": []byte("data")})
	injectSnapTombstone(t, lf, id, tombstone.PhaseLeased)

	dir, _, release, err := lf.DataDir(t.Context(), id)
	if err != nil {
		t.Fatalf("DataDir after leased tombstone: %v", err)
	}
	release()
	if want := lf.conf.SnapshotDataDir(id); dir != want {
		t.Errorf("dir = %s, want %s", dir, want)
	}
	if snapTombstonePresent(t, lf, id) {
		t.Error("leased tombstone not rolled back")
	}
	if _, err := os.Stat(lf.conf.SnapshotDataDir(id)); err != nil {
		t.Errorf("data dir gone after rollback: %v", err)
	}
}

func TestSharedLeaseEscalationDeletingRollsForward(t *testing.T) {
	lf := newTestLF(t)
	id := makeExportableSnapshot(t, lf, "esc-deleting", map[string][]byte{"disk.raw": []byte("data")})
	injectSnapTombstone(t, lf, id, tombstone.PhaseDeleting)

	if _, _, _, err := lf.DataDir(t.Context(), id); !errors.Is(err, snapshot.ErrNotFound) {
		t.Fatalf("DataDir after deleting tombstone = %v, want ErrNotFound", err)
	}
	if snapTombstonePresent(t, lf, id) {
		t.Error("deleting tombstone not finalized")
	}
	if _, err := os.Stat(lf.conf.SnapshotDataDir(id)); !os.IsNotExist(err) {
		t.Errorf("data dir survived roll-forward: %v", err)
	}
	var gone bool
	if err := lf.view(t.Context(), func(tx *snapTx) error {
		rec, err := tx.Get(id)
		gone = rec == nil
		return err
	}); err != nil {
		t.Fatalf("view record: %v", err)
	}
	if !gone {
		t.Error("record survived roll-forward")
	}
	if _, _, _, err := lf.DataDir(t.Context(), "esc-deleting"); !errors.Is(err, snapshot.ErrNotFound) {
		t.Errorf("name still resolves after roll-forward: %v", err)
	}
}

func injectSnapTombstone(t *testing.T, lf *LocalFile, id string, phase tombstone.Phase) {
	t.Helper()
	ctx := t.Context()
	ts := lf.tombstones()
	if err := lf.update(ctx, func(tx *snapTx) error {
		rec, err := tx.Get(id)
		if err != nil {
			return err
		}
		cl, err := tombstone.MarshalCleanup(snapCleanup{Name: rec.Name, DataDir: rec.DataDir})
		if err != nil {
			return err
		}
		leaseID, err := ts.Lease(ctx, tx.Writer(), id, tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cl})
		if err != nil {
			return err
		}
		if phase == tombstone.PhaseDeleting {
			return ts.MarkDeleting(ctx, tx.Writer(), id, leaseID)
		}
		return nil
	}); err != nil {
		t.Fatalf("inject tombstone: %v", err)
	}
}

func snapTombstonePresent(t *testing.T, lf *LocalFile, id string) bool {
	t.Helper()
	var present bool
	if err := lf.view(t.Context(), func(tx *snapTx) error {
		rec, err := lf.tombstones().Get(t.Context(), tx.Reader(), id)
		present = rec != nil
		return err
	}); err != nil {
		t.Fatalf("view tombstone: %v", err)
	}
	return present
}
