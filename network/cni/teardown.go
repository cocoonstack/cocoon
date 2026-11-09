package cni

import (
	"context"
	"encoding/json/v2"
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
		recovered bool
	)
	if err := c.update(ctx, func(t *netTx) error {
		leaseID, cl, recovered = "", netCleanup{}, false
		records, err := t.byVMID(vmID)
		if err != nil {
			return err
		}
		mode := tombstone.ModeAggregate
		if subset != nil {
			mode = tombstone.ModeSubset
			records = slices.DeleteFunc(records, func(r networkRecord) bool { return !slices.Contains(subset, r.ID) })
		}
		for _, r := range records {
			cl.Records = append(cl.Records, netCleanupRecord{ID: r.ID, Type: r.Type, IfName: r.IfName})
		}
		if mode == tombstone.ModeAggregate {
			cl.Netns = c.conf.netnsPath(vmID)
		}
		var resumed *tombstone.Record
		leaseID, resumed, err = ts.Acquire(ctx, t.Writer(), vmID, func() (tombstone.Payload, error) {
			cleanup, mErr := tombstone.MarshalCleanup(cl)
			if mErr != nil {
				return tombstone.Payload{}, mErr
			}
			return tombstone.Payload{Mode: mode, Cleanup: cleanup}, nil
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
		recovered = resumed.Payload.Mode != mode || (mode == tombstone.ModeSubset && !coversRecords(cl, want))
		return nil
	}); err != nil {
		return err
	}
	if err := c.update(ctx, func(t *netTx) error {
		return ts.MarkDeleting(ctx, t.Writer(), vmID, leaseID)
	}); err != nil {
		return err
	}
	if err := c.finishTeardown(ctx, vmID, leaseID, cl, deleteTAP); err != nil {
		return err
	}
	if recovered {
		return fmt.Errorf("vm %s network teardown was interrupted; recovery completed, retry the operation: %w", vmID, meta.ErrConflict)
	}
	return nil
}

// finishTeardown runs the slow CNI DEL / netns work outside any transaction, driven by the payload, then the fenced finalize.
func (c *CNI) finishTeardown(ctx context.Context, vmID, leaseID string, cl netCleanup, deleteTAP bool) error {
	ts := c.tombstones()
	records := make([]networkRecord, 0, len(cl.Records))
	for _, r := range cl.Records {
		records = append(records, networkRecord{ID: r.ID, Type: r.Type, VMID: vmID, IfName: r.IfName})
	}
	// A retry after the netns is gone skips TAP deletion; CNI DEL still releases IPAM.
	nsPath := c.conf.netnsPath(vmID)
	if _, err := statNetnsFn(nsPath); errors.Is(err, fs.ErrNotExist) {
		deleteTAP, nsPath = false, ""
	}
	downIDs, tdErr := c.tearDownNICs(ctx, vmID, nsPath, records, deleteTAP)
	if tdErr == nil && cl.Netns != "" {
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
			return nil
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

// recoverTombstone drives vmID's tombstone under the held VM lock; rolledForward reports a completed deleting recovery so entrypoints refuse the current operation.
func (c *CNI) recoverTombstone(ctx context.Context, vmID string) (rolledForward bool, err error) {
	ts := c.tombstones()
	var (
		rec *tombstone.Record
		cl  netCleanup
	)
	if err := c.update(ctx, func(t *netTx) error {
		var err error
		rec, err = ts.Recover(ctx, t.Writer(), vmID, &cl)
		return err
	}); err != nil {
		return false, err
	}
	if rec == nil {
		return false, nil
	}
	// Subset teardown (vm net remove) creates its TAPs independently of the netns lifetime, so recovery restores Remove's deleteTAP; an aggregate's TAPs die with the netns.
	deleteTAP := rec.Payload.Mode == tombstone.ModeSubset
	if err := c.finishTeardown(ctx, vmID, rec.LeaseID, cl, deleteTAP); err != nil {
		return false, err
	}
	log.WithFunc("cni.recoverTombstone").Warnf(ctx, "rolled forward interrupted teardown for VM %s", vmID)
	return true, nil
}

func coversRecords(cl, want netCleanup) bool {
	return !slices.ContainsFunc(want.Records, func(w netCleanupRecord) bool {
		return !slices.ContainsFunc(cl.Records, func(r netCleanupRecord) bool { return r.ID == w.ID })
	})
}
