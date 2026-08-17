package cni

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/cocoonstack/cocoon/config"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

const (
	bridgeConflist = `{
		"cniVersion": "1.0.0",
		"name": "cni-bridge",
		"plugins": [
			{"type": "bridge", "bridge": "br0"}
		]
	}`
	macvlanConflist = `{
		"cniVersion": "1.0.0",
		"name": "cni-macvlan",
		"plugins": [
			{"type": "macvlan", "master": "eth0"}
		]
	}`
	hostNetConflist = `{
		"cniVersion": "1.0.0",
		"name": "cni-host",
		"plugins": [
			{"type": "host-local"}
		]
	}`
)

var testNetTables = metajson.TableCodec{Specs: []metajson.TableSpec{
	{Key: "networks", Table: TableRecords},
	{Key: "tombstones", Table: tombstone.TableName, Optional: true},
}}

func TestNetnsNameHonorsScope(t *testing.T) {
	for _, tt := range []struct {
		scope string
		want  string
	}{
		{"", "cocoon-vm1"},
		{"mt", "mt-vm1"},
	} {
		c := NewConfig(&config.Config{NetScope: tt.scope})
		if got := c.netnsName("vm1"); got != tt.want {
			t.Errorf("scope %q: netnsName = %q, want %q", tt.scope, got, tt.want)
		}
		if got, want := c.netnsPath("vm1"), filepath.Join(netnsBasePath, tt.want); got != want {
			t.Errorf("scope %q: netnsPath = %q, want %q", tt.scope, got, want)
		}
	}
}

func TestLoadConfLists(t *testing.T) {
	t.Run("empty dir errors", func(t *testing.T) {
		dir := t.TempDir()
		_, _, err := loadConfLists(dir)
		if err == nil {
			t.Fatalf("expected error on empty dir, got nil")
		}
		if !strings.Contains(err.Error(), "no .conflist files") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("non-conflist files ignored", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "10-something.conf"), bridgeConflist)
		writeFile(t, filepath.Join(dir, "20-readme.txt"), "ignored")
		_, _, err := loadConfLists(dir)
		if err == nil {
			t.Fatalf("expected error when only non-.conflist files present")
		}
	})

	t.Run("single conflist becomes default", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "10-bridge.conflist"), bridgeConflist)
		lists, def, err := loadConfLists(dir)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if def != "cni-bridge" {
			t.Errorf("default = %q, want cni-bridge", def)
		}
		if _, ok := lists["cni-bridge"]; !ok {
			t.Errorf("cni-bridge missing from %v", lists)
		}
	})

	t.Run("default is alphabetically first by filename", func(t *testing.T) {
		dir := t.TempDir()
		// libcni.ConfFiles sorts by filename; lex order is 10-, 20-, 30-.
		writeFile(t, filepath.Join(dir, "30-host.conflist"), hostNetConflist)
		writeFile(t, filepath.Join(dir, "10-bridge.conflist"), bridgeConflist)
		writeFile(t, filepath.Join(dir, "20-macvlan.conflist"), macvlanConflist)

		lists, def, err := loadConfLists(dir)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if def != "cni-bridge" {
			t.Errorf("default = %q, want cni-bridge (10- prefix wins)", def)
		}
		for _, want := range []string{"cni-bridge", "cni-macvlan", "cni-host"} {
			if _, ok := lists[want]; !ok {
				t.Errorf("missing conflist %q in %v", want, lists)
			}
		}
		if len(lists) != 3 {
			t.Errorf("got %d conflists, want 3", len(lists))
		}
	})

	t.Run("bad conflist surfaces parse error", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "bad.conflist"), "{not json")
		_, _, err := loadConfLists(dir)
		if err == nil {
			t.Fatalf("expected parse error, got nil")
		}
	})
}

func TestTearDownNICsAttemptsAllRecords(t *testing.T) {
	cl, err := libcni.ConfListFromBytes([]byte(bridgeConflist))
	if err != nil {
		t.Fatal(err)
	}
	exec := &recordingExec{failIf: "eth1"}
	c := &CNI{
		confLists:   map[string]*libcni.NetworkConfigList{"cni-bridge": cl},
		defaultName: "cni-bridge",
		cniConf:     libcni.NewCNIConfig([]string{"/nonexistent"}, exec),
	}
	records := []networkRecord{
		{ID: "n0", Type: "cni-bridge", VMID: "vm1", IfName: "eth0"},
		{ID: "n1", Type: "cni-bridge", VMID: "vm1", IfName: "eth1"},
		{ID: "n2", Type: "cni-bridge", VMID: "vm1", IfName: "eth2"},
	}

	downIDs, err := c.tearDownNICs(t.Context(), "vm1", "/run/netns/vm1", records, false)
	if err == nil || !strings.Contains(err.Error(), "eth1") {
		t.Fatalf("tearDownNICs err = %v, want eth1 failure", err)
	}
	want := []string{"eth0", "eth1", "eth2"}
	if !slices.Equal(exec.attempted, want) {
		t.Fatalf("DEL attempted on %v, want %v (a mid-list failure must not skip later records)", exec.attempted, want)
	}
	if wantDown := []string{"n0", "n2"}; !slices.Equal(downIDs, wantDown) {
		t.Fatalf("downIDs = %v, want %v (only fully-torn-down records are sweepable)", downIDs, wantDown)
	}
}

func TestRemoveKeepsFailedNICRecords(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	exec.failIf = "eth1"
	stubLifecycleSeams(t)

	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth0", "eth1")

	// eth1's CNI DEL fails: its record must survive the sweep so vm rm/GC/retry can still release the IPAM lease; eth0's record must be swept.
	if err := c.Remove(ctx, "vm1", 0, 1); err == nil || !strings.Contains(err.Error(), "eth1") {
		t.Fatalf("Remove err = %v, want eth1 failure", err)
	}
	assertRecordIDs(t, c, []string{"n-eth1"})

	// Retry after the fault clears: the kept record lets Remove finish the release.
	exec.failIf = ""
	if err := c.Remove(ctx, "vm1", 1); err != nil {
		t.Fatalf("retry Remove: %v", err)
	}
	assertRecordIDs(t, c, nil)
}

func TestRemoveSweepsDuplicateIfNameRecords(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)

	ctx := t.Context()
	// A failed reclaim can leave two records for one ifname; Remove must tear down and sweep both (DEL is idempotent), not strand one as a phantom.
	seedRecords(t, c, "vm1", "eth1")
	if err := c.update(ctx, func(t *netTx) error {
		return t.Put("n-eth1-dup", &networkRecord{ID: "n-eth1-dup", Type: "cni-bridge", VMID: "vm1", IfName: "eth1"})
	}); err != nil {
		t.Fatal(err)
	}

	if err := c.Remove(ctx, "vm1", 1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertRecordIDs(t, c, nil)
}

func TestDeleteVMKeepsFailedNICRecords(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	exec.failIf = "eth1"
	stubLifecycleSeams(t)

	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth0", "eth1")

	// A failed NIC release keeps its record AND the netns, surfaced as an error so rm reports the retryable leftover.
	err := c.teardownProtocol(ctx, "vm1", nil, false)
	if err == nil || !strings.Contains(err.Error(), "netns kept") {
		t.Fatalf("deleteVM should surface the incomplete release, got: %v", err)
	}
	assertRecordIDs(t, c, []string{"n-eth1"})
}

func TestDeleteVMZeroNICsWithoutConflist(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	c.cniConf = nil
	stubLifecycleSeams(t)

	// A VM resized to 0 NICs has no lease needing a plugin DEL; a missing conflist must not wedge its netns removal.
	if err := c.teardownProtocol(t.Context(), "vm1", nil, false); err != nil {
		t.Fatalf("deleteVM with zero records: %v", err)
	}
}

func TestVerifyDetectsMissingTAP(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	origStat, origTap := statNetnsFn, tapPresentFn
	statNetnsFn = func(string) (os.FileInfo, error) { return nil, nil }
	tapPresentFn = func(_, tap string) error { return fmt.Errorf("tap %s: not found", tap) }
	t.Cleanup(func() { statNetnsFn, tapPresentFn = origStat, origTap })

	// A half-rolled-back recovery leaves the netns without TAPs; Verify must read that as broken, not healthy.
	expected := []*types.NetworkConfig{{TAP: "tap-vm1-0"}}
	if err := c.Verify(t.Context(), "vm1", expected); err == nil {
		t.Fatal("Verify must fail when an expected TAP is missing")
	}
	tapPresentFn = func(_, _ string) error { return nil }
	if err := c.Verify(t.Context(), "vm1", expected); err != nil {
		t.Fatalf("Verify with all TAPs present: %v", err)
	}
}

func TestReclaimStaleNIC(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	stubLifecycleSeams(t)

	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth1")
	rec := networkRecord{ID: "n-eth1", Type: "cni-bridge", VMID: "vm1", IfName: "eth1"}

	// DEL failure keeps the record (nothing was released).
	exec.failIf = "eth1"
	if err := c.reclaimStaleNIC(ctx, "vm1", "/run/netns/vm1", rec); err == nil {
		t.Fatal("reclaimStaleNIC: want error on failed DEL")
	}
	assertRecordIDs(t, c, []string{"n-eth1"})

	// TAP-delete failure also keeps the record: sweeping would leave a live TAP that collides with the re-add's CreateTAP, with nothing left to retry it.
	exec.failIf = ""
	deleteTAPFn = func(string, string) error { return fmt.Errorf("device busy") }
	if err := c.reclaimStaleNIC(ctx, "vm1", "/run/netns/vm1", rec); err == nil {
		t.Fatal("reclaimStaleNIC: want error on failed TAP delete")
	}
	assertRecordIDs(t, c, []string{"n-eth1"})

	// Both succeed: the record is released and swept, freeing the index for re-add.
	deleteTAPFn = func(string, string) error { return nil }
	if err := c.reclaimStaleNIC(ctx, "vm1", "/run/netns/vm1", rec); err != nil {
		t.Fatalf("reclaimStaleNIC: %v", err)
	}
	assertRecordIDs(t, c, nil)
}

func TestAddFailsClosedOnStaleReclaim(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	stubLifecycleSeams(t)

	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth0")

	// The stale record's DEL fails: Add must fail instead of double-allocating (lenient IPAM) or burying the root cause under the ADD failure (strict IPAM).
	exec.failIf = "eth0"
	if _, err := c.Add(ctx, "vm1", testVMCfg(), network.AddSpec{Index: 0}); err == nil || !strings.Contains(err.Error(), "reclaim stale NIC") {
		t.Fatalf("Add err = %v, want reclaim failure", err)
	}
	assertRecordIDs(t, c, []string{"n-eth0"})

	// Retry after the fault clears: reclaim succeeds, the fresh add replaces the record.
	exec.failIf = ""
	configs, err := c.Add(ctx, "vm1", testVMCfg(), network.AddSpec{Index: 0})
	if err != nil {
		t.Fatalf("retry Add: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("got %d configs, want 1", len(configs))
	}
	var got []networkRecord
	if err := c.view(ctx, func(t *netTx) error {
		var err error
		got, err = t.byVMID("vm1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID == "n-eth0" || got[0].IfName != "eth0" {
		t.Fatalf("records = %+v, want one fresh eth0 record", got)
	}
}

func TestQuiesceSkipsMissingNetns(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	called := false
	origSet := setLinkStateFn
	setLinkStateFn = func(string, []string, bool) error { called = true; return nil }
	origStat := statNetnsFn
	statNetnsFn = func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }
	t.Cleanup(func() { setLinkStateFn, statNetnsFn = origSet, origStat })

	seedRecords(t, c, "vm1", "eth0")

	// A host reboot wipes every netns while the records survive; with no plumbing left, a stop's pending quiesce must settle instead of retrying forever.
	if err := c.Quiesce(t.Context(), "vm1"); err != nil {
		t.Fatalf("Quiesce with a missing netns: %v", err)
	}
	if called {
		t.Error("tried to enter a netns that no longer exists")
	}
}

func TestQuiesceUnquiesceTogglesEveryNIC(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	var gotNS string
	var gotIfs []string
	var gotUp []bool
	origSet := setLinkStateFn
	setLinkStateFn = func(nsPath string, ifNames []string, up bool) error {
		gotNS, gotIfs = nsPath, ifNames
		gotUp = append(gotUp, up)
		return nil
	}
	t.Cleanup(func() { setLinkStateFn = origSet })
	origStat := statNetnsFn
	statNetnsFn = func(string) (os.FileInfo, error) { return nil, nil }
	t.Cleanup(func() { statNetnsFn = origStat })

	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth0", "eth1")

	if err := c.Quiesce(ctx, "vm1"); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if gotNS != c.conf.netnsPath("vm1") {
		t.Fatalf("nsPath = %q, want %q", gotNS, c.conf.netnsPath("vm1"))
	}
	slices.Sort(gotIfs)
	if !slices.Equal(gotIfs, []string{"eth0", "eth1"}) {
		t.Fatalf("ifNames = %v, want every NIC [eth0 eth1]", gotIfs)
	}
	if err := c.Unquiesce(ctx, "vm1"); err != nil {
		t.Fatalf("Unquiesce: %v", err)
	}
	if !slices.Equal(gotUp, []bool{false, true}) {
		t.Fatalf("state sequence = %v, want [false true] (Quiesce down, Unquiesce up)", gotUp)
	}
}

func TestQuiesceNoRecordsSkipsNetns(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	called := false
	origSet := setLinkStateFn
	setLinkStateFn = func(string, []string, bool) error { called = true; return nil }
	t.Cleanup(func() { setLinkStateFn = origSet })

	// A VM with no NIC records owns no plumbing to storm: never enter its netns.
	if err := c.Quiesce(t.Context(), "ghost"); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if called {
		t.Fatal("setLinkStateFn called for a VM with no records")
	}
}

// newTestCNIWithStore builds a CNI over a real JSON store and a recordingExec-backed libcni.
func newTestCNIWithStore(t *testing.T) (*CNI, *recordingExec) {
	t.Helper()
	cl, err := libcni.ConfListFromBytes([]byte(bridgeConflist))
	if err != nil {
		t.Fatal(err)
	}
	exec := &recordingExec{}
	dir := t.TempDir()
	store, err := metajson.Open(metajson.Namespace{
		Name:     NamespaceName,
		FilePath: filepath.Join(dir, "net.json"),
		LockPath: filepath.Join(dir, "net.lock"),
		Codec:    testNetTables,
	})
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &CNI{
		conf:        NewConfig(&config.Config{RootDir: dir}),
		meta:        store,
		confLists:   map[string]*libcni.NetworkConfigList{"cni-bridge": cl},
		defaultName: "cni-bridge",
		cniConf:     libcni.NewCNIConfigWithCacheDir([]string{"/nonexistent"}, filepath.Join(dir, "cache"), exec),
	}, exec
}

// stubLifecycleSeams replaces the linux-only netns/TAP seams with success stubs, matching the real absent-is-success semantics; t.Cleanup restores them.
func stubLifecycleSeams(t *testing.T) {
	t.Helper()
	origTAP, origNetns, origEnsure, origTC := deleteTAPFn, deleteNetnsFn, ensureNetnsFn, setupTCRedirectFn
	origSet := setLinkStateFn
	deleteTAPFn = func(string, string) error { return nil }
	deleteNetnsFn = func(context.Context, string) error { return nil }
	ensureNetnsFn = func(string, string) (bool, error) { return false, nil }
	setupTCRedirectFn = func(_, _, _ string, _ int, _ string) (string, error) { return "aa:bb:cc:dd:ee:01", nil }
	setLinkStateFn = func(string, []string, bool) error { return nil }
	t.Cleanup(func() {
		deleteTAPFn, deleteNetnsFn, ensureNetnsFn, setupTCRedirectFn = origTAP, origNetns, origEnsure, origTC
		setLinkStateFn = origSet
	})
}

func testVMCfg() *types.VMConfig {
	return &types.VMConfig{Config: types.Config{CPU: 2, Network: "cni-bridge"}}
}

// seedRecords inserts one record per ifName, with ID "n-<ifName>".
func seedRecords(t *testing.T, c *CNI, vmID string, ifNames ...string) {
	t.Helper()
	if err := c.update(t.Context(), func(tx *netTx) error {
		for _, ifName := range ifNames {
			id := "n-" + ifName
			if err := tx.Put(id, &networkRecord{ID: id, Type: "cni-bridge", VMID: vmID, IfName: ifName}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRecordIDs(t *testing.T, c *CNI, want []string) {
	t.Helper()
	var got []string
	if err := c.view(t.Context(), func(tx *netTx) error {
		return tx.Scan(func(id string, _ *networkRecord) error {
			got = append(got, id)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
}

// recordingExec fakes CNI plugin execution, recording each DEL's CNI_IFNAME and failing one.
type recordingExec struct {
	attempted []string
	failIf    string
}

func (e *recordingExec) ExecPlugin(_ context.Context, _ string, _ []byte, environ []string) ([]byte, error) {
	var ifName string
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "CNI_IFNAME="); ok {
			ifName = v
		}
	}
	e.attempted = append(e.attempted, ifName)
	if ifName == e.failIf {
		return nil, fmt.Errorf("simulated plugin failure on %s", ifName)
	}
	return []byte(`{"cniVersion":"1.0.0"}`), nil
}

func (e *recordingExec) FindInPath(plugin string, _ []string) (string, error) {
	return "/fake/" + plugin, nil
}

func (e *recordingExec) Decode([]byte) (version.PluginInfo, error) {
	return version.PluginSupports("0.3.1", "0.4.0", "1.0.0"), nil
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
