package cni

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta/tombstone"
)

// netCleanup is the networks-namespace tombstone payload, keyed by VM. An
// AGGREGATE teardown removes every listed record plus the netns; a SUBSET (a
// `vm net remove` resize) removes only the records named here — never NIC
// indices, which cannot disambiguate duplicate rows — and leaves the netns
// and the other NICs alone (design §2).
type netCleanup struct {
	Netns   string             `json:"netns,omitempty"`
	Records []netCleanupRecord `json:"records"`
}

type netCleanupRecord struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	IfName string `json:"if_name"`
}

func (c *CNI) tombstones() *tombstone.Table {
	return tombstone.NewTable(c.meta, metaNS)
}

// teardownProtocol runs the §5 phase protocol for one VM's networking under
// its held VM lock. subset lists the record IDs of a partial teardown; nil
// means aggregate (all records + the netns). deleteTAP mirrors the legacy
// Remove flag.
func (c *CNI) teardownProtocol(ctx context.Context, vmID string, subset []string, deleteTAP bool) error {
	ts := c.tombstones()
	var (
		leaseID string
		cl      netCleanup
		mode    tombstone.Mode
	)
	if err := c.update(ctx, func(t *netTx) error {
		leaseID, cl = "", netCleanup{}
		existing, err := ts.Get(ctx, t.w, vmID)
		if err != nil {
			return err
		}
		if existing != nil {
			taken, takeErr := ts.TakeOver(ctx, t.w, vmID)
			if takeErr != nil {
				return takeErr
			}
			leaseID = taken.LeaseID
			mode = taken.Payload.Mode
			return json.Unmarshal(taken.Payload.Cleanup, &cl)
		}
		records, err := t.byVMID(vmID)
		if err != nil {
			return err
		}
		mode = tombstone.ModeAggregate
		if subset != nil {
			mode = tombstone.ModeSubset
			records = filterRecords(records, subset)
		}
		for _, r := range records {
			cl.Records = append(cl.Records, netCleanupRecord{ID: r.ID, Type: r.Type, IfName: r.IfName})
		}
		if mode == tombstone.ModeAggregate {
			cl.Netns = netnsPath(vmID)
		}
		kind := tombstone.KindRecord
		if len(cl.Records) == 0 {
			kind = tombstone.KindOrphan // a 0-NIC VM still owns its netns
		}
		cleanup, err := tombstone.MarshalCleanup(cl)
		if err != nil {
			return err
		}
		leaseID, err = ts.Lease(ctx, t.w, vmID, tombstone.Payload{Kind: kind, Mode: mode, Cleanup: cleanup})
		return err
	}); err != nil {
		return err
	}
	if err := c.update(ctx, func(t *netTx) error {
		return ts.MarkDeleting(ctx, t.w, vmID, leaseID)
	}); err != nil {
		return err
	}
	return c.finishTeardown(ctx, vmID, leaseID, mode, cl, deleteTAP)
}

// finishTeardown runs the slow CNI DEL / netns work outside any transaction,
// driven by the payload, then the fenced finalize.
func (c *CNI) finishTeardown(ctx context.Context, vmID, leaseID string, mode tombstone.Mode, cl netCleanup, deleteTAP bool) error {
	ts := c.tombstones()
	records := make([]networkRecord, 0, len(cl.Records))
	for _, r := range cl.Records {
		records = append(records, networkRecord{ID: r.ID, Type: r.Type, VMID: vmID, IfName: r.IfName})
	}
	downIDs, tdErr := c.tearDownNICs(ctx, vmID, netnsPath(vmID), records, deleteTAP)
	// Sweep only released records: a failed DEL keeps its record AND the tombstone so the retry resumes with context intact.
	if err := c.update(ctx, func(t *netTx) error {
		for _, id := range downIDs {
			if err := t.del(id); err != nil {
				return err
			}
		}
		if tdErr != nil {
			return nil // keep the tombstone: recovery re-runs the remaining DELs
		}
		if mode == tombstone.ModeAggregate && cl.Netns != "" {
			if err := deleteNetnsFn(ctx, netnsName(vmID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove netns %s: %w", cl.Netns, err)
			}
		}
		err := ts.Finalize(ctx, t.w, vmID, leaseID)
		if errors.Is(err, tombstone.ErrLost) {
			return nil
		}
		return err
	}); err != nil {
		return err
	}
	if tdErr != nil {
		return fmt.Errorf("nic release incomplete, netns kept for retry (tombstone resumes): %w", tdErr)
	}
	return nil
}

// recoverTombstone drives vmID's tombstone under the held VM lock.
func (c *CNI) recoverTombstone(ctx context.Context, vmID string) error {
	ts := c.tombstones()
	var (
		rec     *tombstone.Record
		leaseID string
	)
	if err := c.update(ctx, func(t *netTx) error {
		var err error
		rec, err = ts.Get(ctx, t.w, vmID)
		if err != nil || rec == nil {
			return err
		}
		taken, takeErr := ts.TakeOver(ctx, t.w, vmID)
		if takeErr != nil {
			return takeErr
		}
		leaseID = taken.LeaseID
		if rec.Phase == tombstone.PhaseLeased {
			return ts.Rollback(ctx, t.w, vmID, leaseID)
		}
		return nil
	}); err != nil {
		return err
	}
	if rec == nil || rec.Phase == tombstone.PhaseLeased {
		return nil
	}
	var cl netCleanup
	if err := json.Unmarshal(rec.Payload.Cleanup, &cl); err != nil {
		return fmt.Errorf("tombstone %s payload: %w", vmID, err)
	}
	if err := c.finishTeardown(ctx, vmID, leaseID, rec.Payload.Mode, cl, false); err != nil {
		return err
	}
	log.WithFunc("cni.recoverTombstone").Warnf(ctx, "rolled forward interrupted teardown for VM %s", vmID)
	return nil
}

func filterRecords(records []networkRecord, ids []string) []networkRecord {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]networkRecord, 0, len(ids))
	for _, r := range records {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out
}
