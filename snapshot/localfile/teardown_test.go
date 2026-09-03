package localfile

import (
	"errors"
	"os"
	"testing"

	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/metering"
	meteringcapture "github.com/cocoonstack/cocoon/metering/capture"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
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
	rec := meteringcapture.New()
	lf := newTestLFWithRecorder(t, rec)
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
	entries := rec.Entries()
	if len(entries) == 0 || entries[len(entries)-1].Kind != metering.KindSnapStorageStop || entries[len(entries)-1].SnapshotID != id || entries[len(entries)-1].Reason != metering.ReasonSnapRemove {
		t.Errorf("roll-forward did not close the storage interval: %+v", entries)
	}
}

func TestDeletingPendingRecoverySkipsSnapStorageStop(t *testing.T) {
	rec := meteringcapture.New()
	lf := newTestLFWithRecorder(t, rec)
	id := testID(t)
	if _, err := lf.beginCreate(t.Context(), &types.SnapshotConfig{ID: id, Name: "pending", Hypervisor: "cloud-hypervisor"}); err != nil {
		t.Fatalf("beginCreate: %v", err)
	}
	injectSnapTombstone(t, lf, id, tombstone.PhaseDeleting)

	if err := lf.recoverSnapTombstone(t.Context(), id); err != nil {
		t.Fatalf("recoverSnapTombstone: %v", err)
	}
	if entries := rec.Entries(); len(entries) != 0 {
		t.Errorf("entries = %+v, want no stop for pending snapshot", entries)
	}
}

func TestDeletePendingSkipsSnapStorageStop(t *testing.T) {
	rec := meteringcapture.New()
	lf := newTestLFWithRecorder(t, rec)
	id := testID(t)
	if _, err := lf.beginCreate(t.Context(), &types.SnapshotConfig{ID: id, Name: "pending", Hypervisor: "cloud-hypervisor"}); err != nil {
		t.Fatalf("beginCreate: %v", err)
	}

	if _, err := lf.Delete(t.Context(), []string{id}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if entries := rec.Entries(); len(entries) != 0 {
		t.Errorf("entries = %+v, want no stop for pending snapshot", entries)
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
		cl, err := tombstone.MarshalCleanup(snapCleanup{
			Name: rec.Name, DataDir: rec.DataDir, Hypervisor: rec.Hypervisor, EmitStop: !rec.Pending,
		})
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
