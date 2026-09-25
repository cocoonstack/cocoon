package cni

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
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

func TestExtractNetworkInfoGatewayFallsBackToTheDefaultRoute(t *testing.T) {
	addr := net.IPNet{IP: net.ParseIP("10.22.0.10").To4(), Mask: net.CIDRMask(16, 32)}
	v4Default := net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)}
	v6Default := net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
	tests := []struct {
		name   string
		result *current.Result
		want   string
	}{
		{
			name: "ips gateway wins",
			result: &current.Result{
				IPs:    []*current.IPConfig{{Address: addr, Gateway: net.ParseIP("10.22.0.1")}},
				Routes: []*cnitypes.Route{{Dst: v4Default, GW: net.ParseIP("10.22.0.254")}},
			},
			want: "10.22.0.1",
		},
		{
			name: "routes only",
			result: &current.Result{
				IPs: []*current.IPConfig{{Address: addr}},
				Routes: []*cnitypes.Route{
					{Dst: v6Default, GW: net.ParseIP("fd00::1")},
					{Dst: net.IPNet{IP: net.ParseIP("10.30.0.0").To4(), Mask: net.CIDRMask(16, 32)}, GW: net.ParseIP("10.22.0.2")},
					{Dst: v4Default, GW: net.ParseIP("10.22.0.1")},
				},
			},
			want: "10.22.0.1",
		},
		{
			name:   "no gateway anywhere",
			result: &current.Result{IPs: []*current.IPConfig{{Address: addr}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.result.CNIVersion = "1.0.0"
			info, err := extractNetworkInfo(t.Context(), tt.result)
			if err != nil {
				t.Fatalf("extractNetworkInfo: %v", err)
			}
			if info.IP != "10.22.0.10" || info.Prefix != 16 || info.Gateway != tt.want {
				t.Fatalf("info = %+v, want ip 10.22.0.10/16 gateway %q", info, tt.want)
			}
		})
	}
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

	if err := c.Remove(ctx, "vm1", 0, 1); err == nil || !strings.Contains(err.Error(), "eth1") {
		t.Fatalf("Remove err = %v, want eth1 failure", err)
	}
	assertRecordIDs(t, c, []string{"n-eth1"})

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

	if err := c.teardownProtocol(t.Context(), "vm1", nil, false); err != nil {
		t.Fatalf("deleteVM with zero records: %v", err)
	}
}

func TestVerifyDetectsMissingTAP(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	origStat, origTap := statNetnsFn, tapProvisionedFn
	statNetnsFn = func(string) (os.FileInfo, error) { return nil, nil }
	tapProvisionedFn = func(_, tap string) error { return fmt.Errorf("tap %s: not found", tap) }
	t.Cleanup(func() { statNetnsFn, tapProvisionedFn = origStat, origTap })

	expected := []*types.NetworkConfig{{TAP: "tap-vm1-0"}}
	if err := c.Verify(t.Context(), "vm1", expected); err == nil {
		t.Fatal("Verify must fail when an expected TAP is missing")
	}
	tapProvisionedFn = func(_, _ string) error { return nil }
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

	exec.failIf = "eth1"
	if err := c.reclaimStaleNIC(ctx, "vm1", "/run/netns/vm1", rec); err == nil {
		t.Fatal("reclaimStaleNIC: want error on failed DEL")
	}
	assertRecordIDs(t, c, []string{"n-eth1"})

	exec.failIf = ""
	deleteTAPFn = func(string, string) error { return fmt.Errorf("device busy") }
	if err := c.reclaimStaleNIC(ctx, "vm1", "/run/netns/vm1", rec); err == nil {
		t.Fatal("reclaimStaleNIC: want error on failed TAP delete")
	}
	assertRecordIDs(t, c, []string{"n-eth1"})

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

	exec.failIf = "eth0"
	if _, err := c.Add(ctx, "vm1", testVMCfg(), network.AddSpec{Index: 0}); err == nil || !strings.Contains(err.Error(), "reclaim stale NIC") {
		t.Fatalf("Add err = %v, want reclaim failure", err)
	}
	assertRecordIDs(t, c, []string{"n-eth0"})

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

func TestAddRollsBackTheNICsItAttemptedOnAPartialFailure(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	var deletedTAPs []string
	deleteTAPFn = func(_, tap string) error {
		deletedTAPs = append(deletedTAPs, tap)
		return nil
	}

	exec.failIf = "eth1"
	if _, err := c.Add(t.Context(), "vm1", testVMCfg(), network.AddSpec{Index: 0}, network.AddSpec{Index: 1}); err == nil {
		t.Fatal("Add succeeded, want the eth1 failure")
	}
	if want := []string{"eth0", "eth1", "eth0", "eth1"}; !slices.Equal(exec.attempted, want) {
		t.Fatalf("plugin calls = %v, want %v", exec.attempted, want)
	}
	if want := []string{tapNameForVM("vm1", 0)}; !slices.Equal(deletedTAPs, want) {
		t.Fatalf("deleted TAPs = %v, want %v", deletedTAPs, want)
	}
	var got []networkRecord
	if err := c.view(t.Context(), func(t *netTx) error {
		var err error
		got, err = t.byVMID("vm1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IfName != "eth1" {
		t.Fatalf("records = %+v, want only the eth1 intent kept for GC", got)
	}
}

func TestAddCarriesTAPMTU(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	setupTCRedirectFn = func(_, _, _ string, _ int, _ string) (string, int, error) { return "aa:bb:cc:dd:ee:01", 9000, nil }

	configs, err := c.Add(t.Context(), "vm1", testVMCfg(), network.AddSpec{Index: 0})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(configs) != 1 || configs[0].MTU != 9000 || configs[0].MAC != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("configs = %+v, want one config with MTU 9000", configs)
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

	if err := c.Quiesce(t.Context(), "vm1"); err != nil {
		t.Fatalf("Quiesce with a missing netns: %v", err)
	}
	if called {
		t.Error("tried to enter a netns that no longer exists")
	}
}

func TestAddRecoveryPassesIdentityToPlugin(t *testing.T) {
	for _, tt := range []struct {
		name     string
		existing *types.NetworkConfig
		wantArgs []string
	}{
		{
			name:     "MAC only",
			existing: &types.NetworkConfig{MAC: "02:00:00:00:00:01"},
			wantArgs: []string{"IgnoreUnknown=1", "MAC=02:00:00:00:00:01"},
		},
		{
			name: "MAC and IP",
			existing: &types.NetworkConfig{
				MAC:     "02:00:00:00:00:01",
				Network: &types.Network{IP: "10.22.0.10"},
			},
			wantArgs: []string{"IgnoreUnknown=1", "MAC=02:00:00:00:00:01", "IP=10.22.0.10"},
		},
		{
			name:     "IP only",
			existing: &types.NetworkConfig{Network: &types.Network{IP: "10.22.0.10"}},
			wantArgs: []string{"IgnoreUnknown=1", "IP=10.22.0.10"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			slices.Sort(tt.wantArgs)
			c, exec := newTestCNIWithStore(t)
			stubLifecycleSeams(t)
			seedRecords(t, c, "vm1", "eth0")

			for range 2 {
				exec.addArgs = nil
				if _, err := c.Add(t.Context(), "vm1", testVMCfg(), network.AddRecover([]*types.NetworkConfig{tt.existing})...); err != nil {
					t.Fatalf("recover Add: %v", err)
				}
				if len(exec.addArgs) != 1 {
					t.Fatalf("ADD calls = %d, want 1", len(exec.addArgs))
				}
				got := strings.Split(exec.addArgs[0], ";")
				slices.Sort(got)
				if !slices.Equal(got, tt.wantArgs) {
					t.Fatalf("CNI_ARGS = %v, want %v", got, tt.wantArgs)
				}
				assertRecordIDs(t, c, []string{"n-eth0"})
			}
		})
	}
}

func TestAddRecoveryRecordsAnUnrecordedNIC(t *testing.T) {
	c, _ := newTestCNIWithStore(t)
	stubLifecycleSeams(t)
	seedRecords(t, c, "vm1", "eth0")
	existing := []*types.NetworkConfig{{MAC: "02:00:00:00:00:01"}, {MAC: "02:00:00:00:00:02"}}

	for range 2 {
		if _, err := c.Add(t.Context(), "vm1", testVMCfg(), network.AddRecover(existing)...); err != nil {
			t.Fatalf("recover Add: %v", err)
		}
		var got []networkRecord
		if err := c.view(t.Context(), func(t *netTx) error {
			var err error
			got, err = t.byVMID("vm1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(got, func(a, b networkRecord) int { return strings.Compare(a.IfName, b.IfName) })
		if len(got) != 2 || got[0].ID != "n-eth0" || got[1].IfName != "eth1" || got[1].Type != "cni-bridge" {
			t.Fatalf("records = %+v, want n-eth0 plus one cni-bridge eth1 record", got)
		}
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

	if err := c.Quiesce(t.Context(), "ghost"); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if called {
		t.Fatal("setLinkStateFn called for a VM with no records")
	}
}

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
	origSet, origStat := setLinkStateFn, statNetnsFn
	statNetnsFn = func(string) (os.FileInfo, error) { return nil, nil }
	deleteTAPFn = func(string, string) error { return nil }
	deleteNetnsFn = func(context.Context, string) error { return nil }
	ensureNetnsFn = func(string, string) error { return nil }
	setupTCRedirectFn = func(_, _, _ string, _ int, _ string) (string, int, error) { return "aa:bb:cc:dd:ee:01", 0, nil }
	setLinkStateFn = func(string, []string, bool) error { return nil }
	t.Cleanup(func() {
		deleteTAPFn, deleteNetnsFn, ensureNetnsFn, setupTCRedirectFn = origTAP, origNetns, origEnsure, origTC
		setLinkStateFn, statNetnsFn = origSet, origStat
	})
}

func testVMCfg() *types.VMConfig {
	return &types.VMConfig{CPU: 2, Network: "cni-bridge"}
}

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

type recordingExec struct {
	attempted []string
	addArgs   []string
	failIf    string
}

func (e *recordingExec) ExecPlugin(_ context.Context, _ string, _ []byte, environ []string) ([]byte, error) {
	var ifName, args string
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "CNI_IFNAME="); ok {
			ifName = v
		}
		if v, ok := strings.CutPrefix(kv, "CNI_ARGS="); ok {
			args = v
		}
	}
	if slices.Contains(environ, "CNI_COMMAND=ADD") {
		e.addArgs = append(e.addArgs, args)
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
