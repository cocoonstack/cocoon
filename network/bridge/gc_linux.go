//go:build linux

package bridge

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"
	"github.com/vishvananda/netlink"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/utils"
)

const tapPrefix = "bt"

// bridgeSnapshot holds the set of VM ID prefixes that own bt* TAP devices.
type bridgeSnapshot struct {
	prefixes map[string]struct{}
}

// GCModule returns a GC module that reclaims orphan bt* TAP devices.
// It does not require a Bridge instance — only rootDir for the lock file.
func GCModule(rootDir string) gc.Module[bridgeSnapshot] {
	lockPath := filepath.Join(rootDir, "bridge", "gc.lock")
	_ = utils.EnsureDirs(filepath.Dir(lockPath))

	return gc.Module[bridgeSnapshot]{
		Name:   typ,
		Locker: flock.New(lockPath),
		ReadDB: func(_ context.Context) (bridgeSnapshot, error) {
			snap := bridgeSnapshot{prefixes: make(map[string]struct{})}

			links, err := netlink.LinkList()
			if err != nil {
				return snap, err
			}
			for _, l := range links {
				if prefix, ok := parseTAPName(l.Attrs().Name); ok {
					snap.prefixes[prefix] = struct{}{}
				}
			}
			return snap, nil
		},
		Resolve: func(snap bridgeSnapshot, others map[string]any) []string {
			active := gc.Collect(others, gc.VMIDs)

			// Build set of 8-char prefixes from active VM IDs.
			activePrefixes := make(map[string]struct{}, len(active))
			for id := range active {
				activePrefixes[network.VMIDPrefix(id)] = struct{}{}
			}

			var orphans []string
			for prefix := range snap.prefixes {
				if _, ok := activePrefixes[prefix]; !ok {
					orphans = append(orphans, prefix)
				}
			}
			slices.Sort(orphans)
			return orphans
		},
		Collect: func(ctx context.Context, prefixes []string) error {
			logger := log.WithFunc("bridge.gc.Collect")

			orphanSet := make(map[string]struct{}, len(prefixes))
			for _, p := range prefixes {
				orphanSet[p] = struct{}{}
			}

			links, err := netlink.LinkList()
			if err != nil {
				return err
			}
			for _, l := range links {
				name := l.Attrs().Name
				prefix, ok := parseTAPName(name)
				if !ok {
					continue
				}
				if _, orphan := orphanSet[prefix]; !orphan {
					continue
				}
				if err := netlink.LinkDel(l); err != nil {
					logger.Warnf(ctx, "delete orphan TAP %s: %v", name, err)
				} else {
					logger.Infof(ctx, "collected orphan TAP %s", name)
				}
			}
			return nil
		},
	}
}

// parseTAPName extracts the vmID prefix from a bridge TAP name like "bt<prefix>-<nic>".
// Returns the prefix and true, or ("", false) if the name doesn't match.
func parseTAPName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, tapPrefix)
	if !ok {
		return "", false
	}
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 {
		return "", false
	}
	return rest[:idx], true
}
