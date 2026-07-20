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

// netCleanup is the networks-namespace tombstone payload: aggregate removes
// every listed record plus the netns; subset removes only the named record
// IDs (never NIC indices — they cannot disambiguate duplicate rows).
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
	return tombstone.NewTable(c.meta, NamespaceName)
}

// teardownProtocol runs the delete protocol for one VM's networking under its held VM lock; nil subset means aggregate (all records + the netns).
func (c *CNI) teardownProtocol(ctx context.Context, vmID string, subset []string, deleteTAP bool) error {
	ts := c.tombstones()
	var (
		leaseID string
		cl      netCleanup
		mode    tombstone.Mode
	)
	if err := c.update(ctx, func(t *netTx) error {
		leaseID, cl = "", netCleanup{}
		records, err := t.byVMID(vmID)
		if err != nil {
			return err
		}
		mode = tombstone.ModeAggregate
		if subset != nil {
			mode = tombstone.ModeSubset
			records = filterRecords(records, subset)
		}
		var resumed *tombstone.Record
		leaseID, resumed, err = ts.Acquire(ctx, t.Writer(), vmID, func() (tombstone.Payload, error) {
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
			cleanup, mErr := tombstone.MarshalCleanup(cl)
			if mErr != nil {
				return tombstone.Payload{}, mErr
			}
			return tombstone.Payload{Kind: kind, Mode: mode, Cleanup: cleanup}, nil
		})
		if err != nil || resumed == nil {
			return err
		}
		mode = resumed.Payload.Mode
		cl = netCleanup{}
		return json.Unmarshal(resumed.Payload.Cleanup, &cl)
	}); err != nil {
		return err
	}
	if err := c.update(ctx, func(t *netTx) error {
		return ts.MarkDeleting(ctx, t.Writer(), vmID, leaseID)
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
	// A retry after the netns already went (crash between netns removal and
	// the sweep) skips TAP deletion — the TAPs died with the ns; CNI DEL still
	// runs, releasing IPAM by container ID without entering the ns.
	if _, err := statNetnsFn(netnsPath(vmID)); errors.Is(err, fs.ErrNotExist) {
		deleteTAP = false
	}
	downIDs, tdErr := c.tearDownNICs(ctx, vmID, netnsPath(vmID), records, deleteTAP)
	// Slow cleanup stays outside the transaction (clause 1): the netns goes
	// before the commit so a pure retryable closure never carries side effects.
	if tdErr == nil && mode == tombstone.ModeAggregate && cl.Netns != "" {
		if err := deleteNetnsFn(ctx, netnsName(vmID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove netns %s (tombstone kept, retry resumes): %w", cl.Netns, err)
		}
	}
	// Sweep only released records: a failed DEL keeps its record AND the tombstone so the retry resumes with context intact.
	if err := c.update(ctx, func(t *netTx) error {
		for _, id := range downIDs {
			if err := t.Del(id); err != nil {
				return err
			}
		}
		if tdErr != nil {
			return nil // keep the tombstone: recovery re-runs the remaining DELs
		}
		err := ts.Finalize(ctx, t.Writer(), vmID, leaseID)
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

// recoverTombstone drives vmID's tombstone under the held VM lock;
// rolledForward reports a completed deleting recovery so entrypoints refuse
// the current operation (design §5 binding rule).
func (c *CNI) recoverTombstone(ctx context.Context, vmID string) (rolledForward bool, err error) {
	ts := c.tombstones()
	var (
		rec     *tombstone.Record
		leaseID string
	)
	if err := c.update(ctx, func(t *netTx) error {
		var err error
		rec, leaseID, err = ts.Resume(ctx, t.Writer(), vmID)
		return err
	}); err != nil {
		return false, err
	}
	if rec == nil || rec.Phase == tombstone.PhaseLeased {
		return false, nil
	}
	var cl netCleanup
	if err := json.Unmarshal(rec.Payload.Cleanup, &cl); err != nil {
		return false, fmt.Errorf("tombstone %s payload: %w", vmID, err)
	}
	// Subset teardown (vm net remove) creates its TAPs independently of the
	// netns lifetime, so recovery restores Remove's deleteTAP; an aggregate's
	// TAPs die with the netns.
	deleteTAP := rec.Payload.Mode == tombstone.ModeSubset
	if err := c.finishTeardown(ctx, vmID, leaseID, rec.Payload.Mode, cl, deleteTAP); err != nil {
		return false, err
	}
	log.WithFunc("cni.recoverTombstone").Warnf(ctx, "rolled forward interrupted teardown for VM %s", vmID)
	return true, nil
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
