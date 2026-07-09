package cni

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/cocoonstack/cocoon/lock/flock"
	storejson "github.com/cocoonstack/cocoon/storage/json"
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
	origTAP := deleteTAPFn
	deleteTAPFn = func(string, string) error { return nil }
	t.Cleanup(func() { deleteTAPFn = origTAP })

	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth0", "eth1")

	// eth1's CNI DEL fails: its record must survive the sweep so vm rm/GC/retry can
	// still release the IPAM lease; eth0's record must be swept.
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

func TestDeleteVMKeepsFailedNICRecords(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	exec.failIf = "eth1"
	origNetns := deleteNetnsFn
	deleteNetnsFn = func(context.Context, string) error { return nil }
	t.Cleanup(func() { deleteNetnsFn = origNetns })

	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth0", "eth1")

	// vm rm stays best-effort, but the failed NIC's record must survive for GC to retry.
	if err := c.deleteVM(ctx, "vm1"); err != nil {
		t.Fatalf("deleteVM: %v", err)
	}
	assertRecordIDs(t, c, []string{"n-eth1"})
}

func TestReclaimStaleNIC(t *testing.T) {
	c, exec := newTestCNIWithStore(t)
	origTAP := deleteTAPFn
	deleteTAPFn = func(string, string) error { return nil }
	t.Cleanup(func() { deleteTAPFn = origTAP })

	ctx := t.Context()
	seedRecords(t, c, "vm1", "eth1")
	rec := networkRecord{ID: "n-eth1", Type: "cni-bridge", VMID: "vm1", IfName: "eth1"}

	// DEL failure keeps the record (nothing was released).
	exec.failIf = "eth1"
	if err := c.reclaimStaleNIC(ctx, "vm1", "/run/netns/vm1", "tapvm1-1", rec); err == nil {
		t.Fatal("reclaimStaleNIC: want error on failed DEL")
	}
	assertRecordIDs(t, c, []string{"n-eth1"})

	// DEL success releases and sweeps the record, freeing the index for re-add.
	exec.failIf = ""
	if err := c.reclaimStaleNIC(ctx, "vm1", "/run/netns/vm1", "tapvm1-1", rec); err != nil {
		t.Fatalf("reclaimStaleNIC: %v", err)
	}
	assertRecordIDs(t, c, nil)
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
	return &CNI{
		store:       storejson.New[networkIndex](filepath.Join(dir, "net.json"), flock.New(filepath.Join(dir, "net.lock"))),
		confLists:   map[string]*libcni.NetworkConfigList{"cni-bridge": cl},
		defaultName: "cni-bridge",
		cniConf:     libcni.NewCNIConfig([]string{"/nonexistent"}, exec),
	}, exec
}

// seedRecords inserts one record per ifName, with ID "n-<ifName>".
func seedRecords(t *testing.T, c *CNI, vmID string, ifNames ...string) {
	t.Helper()
	if err := c.store.Update(t.Context(), func(idx *networkIndex) error {
		for _, ifName := range ifNames {
			id := "n-" + ifName
			idx.Networks[id] = &networkRecord{ID: id, Type: "cni-bridge", VMID: vmID, IfName: ifName}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRecordIDs(t *testing.T, c *CNI, want []string) {
	t.Helper()
	var got []string
	if err := c.store.With(t.Context(), func(idx *networkIndex) error {
		got = slices.Sorted(maps.Keys(idx.Networks))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
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
		return nil, fmt.Errorf("simulated DEL failure on %s", ifName)
	}
	return []byte(`{}`), nil
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
