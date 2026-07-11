//go:build linux

package bridge

import (
	"context"
	"maps"
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

// GCModule returns a GC module reclaiming orphan bt* TAP devices; it needs no Bridge instance, only rootDir for the lock file.
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
		Resolve: func(_ context.Context, snap bridgeSnapshot, others map[string]any) []string {
			active := gc.Collect(others, gc.VMIDs)

			activePrefixes := make(map[string]struct{}, len(active))
			for id := range active {
				activePrefixes[network.VMIDPrefix(id)] = struct{}{}
			}

			orphans := utils.FilterUnreferenced(slices.Collect(maps.Keys(snap.prefixes)), activePrefixes)
			slices.Sort(orphans)
			return orphans
		},
		Collect: func(ctx context.Context, prefixes []string, _ bridgeSnapshot) error {
			logger := log.WithFunc("gc.bridge")

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
					logger.Infof(ctx, "collected id=%s iface=%s reason=orphan-tap", prefix, name)
				}
			}
			return nil
		},
	}
}

// parseTAPName extracts the vmID prefix from a bridge TAP name like "bt<prefix>-<nic>".
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
