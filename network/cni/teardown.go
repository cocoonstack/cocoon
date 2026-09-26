package cni

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/tombstone"
)

// netCleanup is the networks-namespace tombstone payload; it names record IDs, never NIC indices, which cannot disambiguate duplicate rows.
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
	return tombstone.NewTable(NamespaceName)
}

// teardownProtocol runs the delete protocol for one VM's networking under its held VM lock; nil subset means aggregate (all records + the netns).
func (c *CNI) teardownProtocol(ctx context.Context, vmID string, subset []string, deleteTAP bool) error {
	ts := c.tombstones()
	var (
		leaseID   string
		cl        netCleanup
		mode      tombstone.Mode
		recovered bool
	)
	if err := c.update(ctx, func(t *netTx) error {
		leaseID, cl, recovered = "", netCleanup{}, false
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
			cl.Netns = c.conf.netnsPath(vmID)
		}
		var resumed *tombstone.Record
		leaseID, resumed, err = ts.Acquire(ctx, t.Writer(), vmID, func() (tombstone.Payload, error) {
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
		want := cl
		cl = netCleanup{}
		if err := json.Unmarshal(resumed.Payload.Cleanup, &cl); err != nil {
			return err
		}
		// A retry of the interrupted operation resumes it; any other intent is finished first and refused, since its outcome is not this call's.
		recovered = resumed.Payload.Mode != mode || (mode == tombstone.ModeSubset && !subsetOf(recordIDs(want), recordIDs(cl)))
		mode = resumed.Payload.Mode
		return nil
	}); err != nil {
		return err
	}
	if err := c.update(ctx, func(t *netTx) error {
		return ts.MarkDeleting(ctx, t.Writer(), vmID, leaseID)
	}); err != nil {
		return err
	}
	if err := c.finishTeardown(ctx, vmID, leaseID, mode, cl, deleteTAP); err != nil {
		return err
	}
	if recovered {
		return fmt.Errorf("vm %s network teardown was interrupted; recovery completed, retry the operation: %w", vmID, meta.ErrConflict)
	}
	return nil
}

// finishTeardown runs the slow CNI DEL / netns work outside any transaction, driven by the payload, then the fenced finalize.
func (c *CNI) finishTeardown(ctx context.Context, vmID, leaseID string, mode tombstone.Mode, cl netCleanup, deleteTAP bool) error {
	ts := c.tombstones()
	records := make([]networkRecord, 0, len(cl.Records))
	for _, r := range cl.Records {
		records = append(records, networkRecord{ID: r.ID, Type: r.Type, VMID: vmID, IfName: r.IfName})
	}
	// A retry after the netns already went (crash between netns removal and the sweep) skips TAP deletion — the TAPs died with the ns; CNI DEL still runs, releasing IPAM by container ID without entering the ns.
	nsPath := c.conf.netnsPath(vmID)
	if _, err := statNetnsFn(nsPath); errors.Is(err, fs.ErrNotExist) {
		deleteTAP, nsPath = false, ""
	}
	downIDs, tdErr := c.tearDownNICs(ctx, vmID, nsPath, records, deleteTAP)
	// Slow cleanup stays outside the transaction (clause 1): the netns goes before the commit so a pure retryable closure never carries side effects.
	if tdErr == nil && mode == tombstone.ModeAggregate && cl.Netns != "" {
		if err := deleteNetnsFn(ctx, c.conf.netnsName(vmID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
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

// recoverTombstone drives vmID's tombstone under the held VM lock; rolledForward reports a completed deleting recovery so entrypoints refuse the current operation (design §5 binding rule).
func (c *CNI) recoverTombstone(ctx context.Context, vmID string) (rolledForward bool, err error) {
	ts := c.tombstones()
	var (
		rec     *tombstone.Record
		leaseID string
		cl      netCleanup
	)
	if err := c.update(ctx, func(t *netTx) error {
		var err error
		rec, leaseID, err = ts.Recover(ctx, t.Writer(), vmID, &cl)
		return err
	}); err != nil {
		return false, err
	}
	if rec == nil {
		return false, nil
	}
	// Subset teardown (vm net remove) creates its TAPs independently of the netns lifetime, so recovery restores Remove's deleteTAP; an aggregate's TAPs die with the netns.
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
	return slices.DeleteFunc(records, func(r networkRecord) bool { return !want[r.ID] })
}

func recordIDs(cl netCleanup) []string {
	ids := make([]string, 0, len(cl.Records))
	for _, r := range cl.Records {
		ids = append(ids, r.ID)
	}
	return ids
}

func subsetOf(ids, of []string) bool {
	return !slices.ContainsFunc(ids, func(id string) bool { return !slices.Contains(of, id) })
}
