package cni

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/network"
)

func TestSubsetTeardownRecovery(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	netnsRemoved := 0
	var tapsDeleted []string
	swap(t, &deleteNetnsFn, func(context.Context, string) error { netnsRemoved++; return nil })
	swap(t, &deleteTAPFn, func(_, tap string) error { tapsDeleted = append(tapsDeleted, tap); return nil })
	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth0", "eth1")

	markDeleting(t, c, "vm1", tombstone.ModeSubset, netCleanup{Records: []netCleanupRecord{{ID: "n-eth1", Type: "cni-bridge", IfName: "eth1"}}})

	if _, err := c.recoverTombstone(ctx, "vm1"); err != nil {
		t.Fatalf("recover: %v", err)
	}

	assertRecordIDs(t, c, []string{"n-eth0"})
	if slices.Contains(exec.attempted, "eth0") {
		t.Fatal("subset recovery must not touch the other NIC")
	}
	if netnsRemoved != 0 {
		t.Fatalf("subset recovery removed the netns (%d removals)", netnsRemoved)
	}

	if want := []string{tapNameForVM("vm1", 1)}; !slices.Equal(tapsDeleted, want) {
		t.Fatalf("subset recovery TAP deletions = %v, want %v", tapsDeleted, want)
	}
}

func TestAggregateTeardownRecovery(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	ctx := t.Context()
	seedRecords(t, c, "vm2", "eth0", "eth1")

	if err := c.teardownProtocol(ctx, "vm2", nil, false); err != nil {
		t.Fatalf("aggregate teardown: %v", err)
	}
	assertRecordIDs(t, c, nil)
	left := tombstoneOf(t, c, "vm2")
	if left != nil {
		t.Fatalf("tombstone survived finalize: %+v", left)
	}
}

func TestAggregateRecoveryAfterNetnsGone(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	tapDeletes := 0
	swap(t, &deleteTAPFn, func(string, string) error { tapDeletes++; return nil })
	swap(t, &statNetnsFn, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
	ctx := t.Context()
	seedRecords(t, c, "vm5", "eth0")

	markDeleting(t, c, "vm5", tombstone.ModeAggregate, netCleanup{Netns: "vm5", Records: []netCleanupRecord{{ID: "n-eth0", Type: "cni-bridge", IfName: "eth0"}}})

	if _, err := c.recoverTombstone(ctx, "vm5"); err != nil {
		t.Fatalf("recover with netns gone: %v", err)
	}
	assertRecordIDs(t, c, nil)
	if tapDeletes != 0 {
		t.Fatalf("TAP delete attempted against a removed netns (%d)", tapDeletes)
	}
	left := tombstoneOf(t, c, "vm5")
	if left != nil {
		t.Fatalf("tombstone survived converged recovery: %+v", left)
	}
}

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
	left := tombstoneOf(t, c, "vm3")
	if left == nil || left.Phase != tombstone.PhaseDeleting {
		t.Fatalf("tombstone must stay in deleting: %+v", left)
	}

	exec.failIf = ""
	if err := c.teardownProtocol(ctx, "vm3", []string{"n-eth1"}, false); err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertRecordIDs(t, c, []string{"n-eth0"})
}

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
	c := &CNI{meta: openNetStore(t, dir, "networks")}

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
		if err := tx.Del("NET2"); err != nil {
			return err
		}
		rec, err := tx.Get("NET3")
		if err != nil {
			return err
		}
		rec.IP = "10.0.0.9"
		return tx.Put("NET3", rec)
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

func TestAddRefusesAfterAggregateRollForward(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	ctx := t.Context()
	seedRecords(t, c, "vm6", "eth0")

	markDeleting(t, c, "vm6", tombstone.ModeAggregate, netCleanup{Netns: "vm6", Records: []netCleanupRecord{{ID: "n-eth0", Type: "cni-bridge", IfName: "eth0"}}})

	if _, err := c.Add(ctx, "vm6", testVMCfg(), network.AddSpec{Index: 0}); !errors.Is(err, meta.ErrConflict) {
		t.Fatalf("Add after aggregate roll-forward must refuse with ErrConflict, got %v", err)
	}
	assertRecordIDs(t, c, nil)

	if _, err := c.Add(ctx, "vm6", testVMCfg(), network.AddSpec{Index: 0}); err != nil {
		t.Fatalf("retry on the clean slate: %v", err)
	}
}

func TestOpsRefuseAfterSubsetRollForward(t *testing.T) {
	for _, tt := range []struct {
		name       string
		op         func(context.Context, *CNI) error
		checkRetry bool
	}{
		{"Delete over a subset tombstone", func(ctx context.Context, c *CNI) error { return c.Delete(ctx, "vm1") }, true},
		{"Add after subset roll-forward", func(ctx context.Context, c *CNI) error {
			_, err := c.Add(ctx, "vm1", testVMCfg(), network.AddSpec{Index: 2})
			return err
		}, false},
		{"Remove of eth0 over an eth1 tombstone", func(ctx context.Context, c *CNI) error { return c.Remove(ctx, "vm1", 0) }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestCNIWithStore(t)
			stubLifecycleSeams(t)
			ctx := t.Context()
			seedRecords(t, c, "vm1", "eth0", "eth1")
			markDeleting(t, c, "vm1", tombstone.ModeSubset, netCleanup{Records: []netCleanupRecord{{ID: "n-eth1", Type: "cni-bridge", IfName: "eth1"}}})

			if err := tt.op(ctx, c); !errors.Is(err, meta.ErrConflict) {
				t.Fatalf("%s must finish it and refuse with ErrConflict, got %v", tt.name, err)
			}
			assertRecordIDs(t, c, []string{"n-eth0"})

			if err := tt.op(ctx, c); err != nil {
				t.Fatalf("retry after recovery: %v", err)
			}
			if tt.checkRetry {
				assertRecordIDs(t, c, nil)
			}
		})
	}
}

func tombstoneOf(t *testing.T, c *CNI, vmID string) *tombstone.Record {
	t.Helper()
	ctx := t.Context()
	var rec *tombstone.Record
	if err := c.view(ctx, func(tx *netTx) error {
		var err error
		rec, err = c.tombstones().Get(ctx, tx.Reader(), vmID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rec
}

func markDeleting(t *testing.T, c *CNI, vmID string, mode tombstone.Mode, cl netCleanup) {
	t.Helper()
	ctx := t.Context()
	ts := c.tombstones()
	var leaseID string
	if err := c.update(ctx, func(tx *netTx) error {
		cleanup, err := tombstone.MarshalCleanup(cl)
		if err != nil {
			return err
		}
		leaseID, err = ts.Lease(ctx, tx.Writer(), vmID, tombstone.Payload{Mode: mode, Cleanup: cleanup})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.update(ctx, func(tx *netTx) error {
		return ts.MarkDeleting(ctx, tx.Writer(), vmID, leaseID)
	}); err != nil {
		t.Fatal(err)
	}
}
