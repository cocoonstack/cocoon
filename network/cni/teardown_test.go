package cni

import (
	"context"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/meta/tombstone"
)

// TestSubsetTeardownRecovery is the design §9 subset gate: crash mid-deleting
// on a one-NIC `vm net remove`, then recover — the untouched NIC row must
// survive and the netns must not be removed. An aggregate-shaped test passes
// while this fails, which is why it exists separately.
func TestSubsetTeardownRecovery(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	netnsRemoved := 0
	origNetns := deleteNetnsFn
	deleteNetnsFn = func(context.Context, string) error { netnsRemoved++; return nil }
	t.Cleanup(func() { deleteNetnsFn = origNetns })
	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth0", "eth1")

	// Lease + deleting for the eth1 subset, then "crash" before any DEL ran.
	ts := c.tombstones()
	var leaseID string
	if err := c.update(ctx, func(tx *netTx) error {
		cleanup, err := tombstone.MarshalCleanup(netCleanup{Records: []netCleanupRecord{{ID: "n-eth1", Type: "cni-bridge", IfName: "eth1"}}})
		if err != nil {
			return err
		}
		leaseID, err = ts.Lease(ctx, tx.w, "vm1", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeSubset, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.update(ctx, func(tx *netTx) error {
		return ts.MarkDeleting(ctx, tx.w, "vm1", leaseID)
	}); err != nil {
		t.Fatal(err)
	}

	if err := c.recoverTombstone(ctx, "vm1"); err != nil {
		t.Fatalf("recover: %v", err)
	}
	// eth1's row is gone; eth0's row survives; no netns removal was attempted.
	assertRecordIDs(t, c, []string{"n-eth0"})
	for _, name := range exec.attempted {
		if name == "eth0" {
			t.Fatal("subset recovery must not touch the other NIC")
		}
	}
	if netnsRemoved != 0 {
		t.Fatalf("subset recovery removed the netns (%d removals)", netnsRemoved)
	}
}

// TestAggregateTeardownRecovery rolls a deleting aggregate forward: all rows
// and the netns go.
func TestAggregateTeardownRecovery(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	ctx := t.Context()
	seedRecords(t, c, "vm2", "eth0", "eth1")

	if err := c.teardownProtocol(ctx, "vm2", nil, false); err != nil {
		t.Fatalf("aggregate teardown: %v", err)
	}
	assertRecordIDs(t, c, nil)
	var left *tombstone.Record
	if err := c.view(ctx, func(tx *netTx) error {
		var err error
		left, err = c.tombstones().Get(ctx, tx.r, "vm2")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if left != nil {
		t.Fatalf("tombstone survived finalize: %+v", left)
	}
}

// TestSubsetFailureKeepsTombstone pins the retry story: a failing DEL keeps
// the row and the subset tombstone, and the error names the retry path.
func TestSubsetFailureKeepsTombstone(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	exec.failIf = "eth1"
	stubLifecycleSeams(t)
	ctx := t.Context()
	seedRecords(t, c, "vm3", "eth0", "eth1")

	err := c.teardownProtocol(ctx, "vm3", []string{"n-eth1"}, false)
	if err == nil || !strings.Contains(err.Error(), "eth1") {
		t.Fatalf("want eth1 failure, got %v", err)
	}
	assertRecordIDs(t, c, []string{"n-eth0", "n-eth1"})
	var left *tombstone.Record
	if err := c.view(ctx, func(tx *netTx) error {
		var lerr error
		left, lerr = c.tombstones().Get(ctx, tx.r, "vm3")
		return lerr
	}); err != nil {
		t.Fatal(err)
	}
	if left == nil || left.Phase != tombstone.PhaseDeleting {
		t.Fatalf("tombstone must stay in deleting: %+v", left)
	}
	// Fault clears; the retry converges.
	exec.failIf = ""
	if err := c.teardownProtocol(ctx, "vm3", []string{"n-eth1"}, false); err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertRecordIDs(t, c, []string{"n-eth0"})
}
