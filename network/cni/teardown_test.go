package cni

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/network"
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

	if _, err := c.recoverTombstone(ctx, "vm1"); err != nil {
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

// TestAggregateRecoveryAfterNetnsGone converges the crash window between
// netns removal and the record sweep: TAP deletion is skipped (the TAPs died
// with the ns), DELs still run, rows sweep, the tombstone finalizes.
func TestAggregateRecoveryAfterNetnsGone(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	tapDeletes := 0
	origTAP, origStat := deleteTAPFn, statNetnsFn
	deleteTAPFn = func(string, string) error { tapDeletes++; return nil }
	statNetnsFn = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { deleteTAPFn, statNetnsFn = origTAP, origStat })
	ctx := t.Context()
	seedRecords(t, c, "vm5", "eth0")

	ts := c.tombstones()
	var leaseID string
	if err := c.update(ctx, func(tx *netTx) error {
		cleanup, err := tombstone.MarshalCleanup(netCleanup{Netns: "vm5", Records: []netCleanupRecord{{ID: "n-eth0", Type: "cni-bridge", IfName: "eth0"}}})
		if err != nil {
			return err
		}
		leaseID, err = ts.Lease(ctx, tx.w, "vm5", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.update(ctx, func(tx *netTx) error {
		return ts.MarkDeleting(ctx, tx.w, "vm5", leaseID)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.recoverTombstone(ctx, "vm5"); err != nil {
		t.Fatalf("recover with netns gone: %v", err)
	}
	assertRecordIDs(t, c, nil)
	if tapDeletes != 0 {
		t.Fatalf("TAP delete attempted against a removed netns (%d)", tapDeletes)
	}
	var left *tombstone.Record
	if err := c.view(ctx, func(tx *netTx) error {
		var err error
		left, err = ts.Get(ctx, tx.r, "vm5")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if left != nil {
		t.Fatalf("tombstone survived converged recovery: %+v", left)
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

// TestLegacyDifferentialTrace replays the fixture op sequence over meta-json
// and requires byte-identical output to the legacy storage layer's writes.
func TestLegacyDifferentialTrace(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	baseline, err := os.ReadFile("testdata/legacy-networks.baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile("testdata/legacy-networks.after.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "networks.json")
	if err := os.WriteFile(path, baseline, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := metajson.Open(metajson.Namespace{
		Name: metaNS, FilePath: path, LockPath: filepath.Join(dir, "networks.lock"), Codec: netIndexCodec{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := &CNI{meta: store}

	if err := c.update(ctx, func(*netTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(baseline) {
		t.Fatalf("round-trip not byte-identical:\n got: %s\nwant: %s", got, baseline)
	}

	if err := c.update(ctx, func(tx *netTx) error {
		if err := tx.del("NET2"); err != nil {
			return err
		}
		rec, err := tx.get("NET3")
		if err != nil {
			return err
		}
		rec.IP = "10.0.0.9"
		return tx.put("NET3", rec)
	}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(after) {
		t.Fatalf("differential trace diverged:\n got: %s\nwant: %s", got, after)
	}
}

// TestAddRefusesAfterAggregateRollForward pins the entry rule: recovery of a
// whole-VM deleting tombstone completes the teardown AND refuses the Add; a
// retry then proceeds on the clean slate.
func TestAddRefusesAfterAggregateRollForward(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	ctx := t.Context()
	seedRecords(t, c, "vm6", "eth0")

	ts := c.tombstones()
	var leaseID string
	if err := c.update(ctx, func(tx *netTx) error {
		cleanup, err := tombstone.MarshalCleanup(netCleanup{Netns: "vm6", Records: []netCleanupRecord{{ID: "n-eth0", Type: "cni-bridge", IfName: "eth0"}}})
		if err != nil {
			return err
		}
		leaseID, err = ts.Lease(ctx, tx.w, "vm6", tombstone.Payload{Kind: tombstone.KindRecord, Mode: tombstone.ModeAggregate, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.update(ctx, func(tx *netTx) error {
		return ts.MarkDeleting(ctx, tx.w, "vm6", leaseID)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Add(ctx, "vm6", testVMCfg(), network.AddSpec{Index: 0}); err == nil || !strings.Contains(err.Error(), "mid-delete") {
		t.Fatalf("Add after aggregate roll-forward must refuse, got %v", err)
	}
	assertRecordIDs(t, c, nil)

	if _, err := c.Add(ctx, "vm6", testVMCfg(), network.AddSpec{Index: 0}); err != nil {
		t.Fatalf("retry on the clean slate: %v", err)
	}
}
