package cni

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/containernetworking/cni/libcni"
	cns "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDeleteNetnsCollectsAnUnmountedEntry(t *testing.T) {
	requireNetnsAdmin(t)
	name := "cocoon-stale-" + strconv.Itoa(os.Getpid())
	path := filepath.Join(netnsBasePath, name)
	writeStaleNetnsEntry(t, path)

	if err := deleteNetns(t.Context(), name); err != nil {
		t.Fatalf("deleteNetns: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale netns entry survived: stat err = %v", err)
	}
}

func TestTCRedirectConvergesOnPartialPlumbing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for netns and netlink")
	}
	name := "cocoon-tc-" + strconv.Itoa(os.Getpid())
	if err := createNetns(name); err != nil {
		t.Skipf("create netns: %v", err)
	}
	t.Cleanup(func() { _ = deleteNetns(context.WithoutCancel(t.Context()), name) })
	nsPath := filepath.Join(netnsBasePath, name)
	err := cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		return netlink.LinkAdd(&netlink.Veth{Name: "eth0", PeerName: "peer0"})
	})
	if err != nil {
		t.Fatalf("add veth: %v", err)
	}

	for range 2 {
		if _, _, err := setupTCRedirect(nsPath, "eth0", "tap0", 1, ""); err != nil {
			t.Fatalf("setupTCRedirect: %v", err)
		}
	}
	if err := tapProvisionedInNetns(nsPath, "tap0"); err != nil {
		t.Fatalf("tap with both redirects must verify: %v", err)
	}
	for _, dev := range []string{"eth0", "tap0"} {
		if n := ingressFilterCount(t, nsPath, dev); n != 1 {
			t.Fatalf("%s carries %d ingress filters after two setups, want 1", dev, n)
		}
	}

	err = cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		link, err := netlink.LinkByName("tap0")
		if err != nil {
			return err
		}
		filters, err := netlink.FilterList(link, netlink.HANDLE_INGRESS)
		if err != nil {
			return err
		}
		return netlink.FilterDel(filters[0])
	})
	if err != nil {
		t.Fatalf("drop tap redirect: %v", err)
	}
	if err := tapProvisionedInNetns(nsPath, "tap0"); err == nil {
		t.Fatal("tap without its ingress redirect must not verify")
	}
}

func TestEnsureNetnsReplacesAStaleEntry(t *testing.T) {
	requireNetnsAdmin(t)
	name := "cocoon-ensure-" + strconv.Itoa(os.Getpid())
	path := filepath.Join(netnsBasePath, name)
	writeStaleNetnsEntry(t, path)
	t.Cleanup(func() { _ = deleteNetns(context.WithoutCancel(t.Context()), name) })

	if err := ensureNetns(name, path); err != nil {
		t.Fatalf("ensureNetns: %v", err)
	}
	if err := cns.IsNSorErr(path); err != nil {
		t.Fatalf("stale entry not replaced by a netns: %v", err)
	}
}

func TestDeleteConvergesOnAStaleNetnsEntry(t *testing.T) {
	requireNetnsAdmin(t)
	c, _ := newTestCNIWithStore(t)
	c.cniConf = libcni.NewCNIConfigWithCacheDir([]string{"/nonexistent"}, filepath.Join(t.TempDir(), "cache"), &netnsCheckingExec{})
	vmID := "stale-" + strconv.Itoa(os.Getpid())
	path := c.conf.netnsPath(vmID)
	writeStaleNetnsEntry(t, path)
	seedRecords(t, c, vmID, "eth0")

	if err := c.Delete(t.Context(), vmID); err != nil {
		t.Fatalf("Delete over a stale netns entry: %v", err)
	}
	assertRecordIDs(t, c, nil)
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale netns entry survived: stat err = %v", err)
	}
}

func requireNetnsAdmin(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to write " + netnsBasePath)
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if err := unix.Unmount(probe, 0); errors.Is(err, unix.EPERM) {
		t.Skip("needs CAP_SYS_ADMIN to unmount a netns entry")
	}
}

func writeStaleNetnsEntry(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(netnsBasePath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", netnsBasePath, err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

func ingressFilterCount(t *testing.T, nsPath, dev string) int {
	t.Helper()
	var n int
	err := cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		link, err := netlink.LinkByName(dev)
		if err != nil {
			return err
		}
		filters, err := netlink.FilterList(link, netlink.HANDLE_INGRESS)
		n = len(filters)
		return err
	})
	if err != nil {
		t.Fatalf("list ingress filters on %s: %v", dev, err)
	}
	return n
}

type netnsCheckingExec struct {
	recordingExec
}

func (e *netnsCheckingExec) ExecPlugin(ctx context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
	for _, kv := range environ {
		if nsPath, ok := strings.CutPrefix(kv, "CNI_NETNS="); ok && nsPath != "" {
			if _, notNS := errors.AsType[cns.NSPathNotNSErr](cns.IsNSorErr(nsPath)); notNS {
				return nil, fmt.Errorf("%s is not a netns", nsPath)
			}
		}
	}
	return e.recordingExec.ExecPlugin(ctx, pluginPath, stdinData, environ)
}
