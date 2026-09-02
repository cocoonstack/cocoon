//go:build linux

package bridge

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"
	"github.com/vishvananda/netlink"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/utils"
)

type bridgeSnapshot struct {
	prefixes map[string]struct{}
}

// GCModule returns a GC module reclaiming orphan TAP devices under tapPrefix; it needs no Bridge instance.
func GCModule(tapPrefix string) gc.Module[bridgeSnapshot] {
	return gc.Module[bridgeSnapshot]{
		Name: typ,
		ReadDB: func(_ context.Context) (bridgeSnapshot, error) {
			snap := bridgeSnapshot{prefixes: make(map[string]struct{})}

			links, err := netlink.LinkList()
			if err != nil {
				return snap, err
			}
			for _, l := range links {
				if prefix, ok := parseTAPName(tapPrefix, l.Attrs().Name); ok {
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

			return utils.FilterUnreferenced(slices.Sorted(maps.Keys(snap.prefixes)), activePrefixes)
		},
		Collect: func(ctx context.Context, prefixes []string, _ bridgeSnapshot) error {
			logger := log.WithFunc("gc.bridge")

			if len(prefixes) == 0 {
				return nil
			}
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
				prefix, ok := parseTAPName(tapPrefix, name)
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

// parseTAPName extracts the vmID prefix from a bridge TAP name "<tapPrefix><vmid8>-<nic>".
func parseTAPName(tapPrefix, name string) (string, bool) {
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
