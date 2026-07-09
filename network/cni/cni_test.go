package cni

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/version"
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

	err = c.tearDownNICs(t.Context(), "vm1", "/run/netns/vm1", records, false)
	if err == nil || !strings.Contains(err.Error(), "eth1") {
		t.Fatalf("tearDownNICs err = %v, want eth1 failure", err)
	}
	want := []string{"eth0", "eth1", "eth2"}
	if !slices.Equal(exec.attempted, want) {
		t.Fatalf("DEL attempted on %v, want %v (a mid-list failure must not skip later records)", exec.attempted, want)
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
