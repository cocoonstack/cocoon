//go:build linux

package bridge

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"
	"github.com/vishvananda/netlink"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/utils"
)

var (
	listLinksFn  = netlink.LinkList
	deleteLinkFn = netlink.LinkDel
)

type bridgeSnapshot struct {
	prefixes map[string]struct{}
}

// GCModule returns a GC module reclaiming orphan TAP devices under tapPrefix; it needs no Bridge instance.
func GCModule(tapPrefix string, vmInUse network.VMInUse) gc.Module[bridgeSnapshot] {
	return gc.Module[bridgeSnapshot]{
		Name: typ,
		ReadDB: func(_ context.Context) (bridgeSnapshot, error) {
			snap := bridgeSnapshot{prefixes: make(map[string]struct{})}

			links, err := listLinksFn()
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
			links, err := listLinksFn()
			if err != nil {
				return err
			}
			// Group first: one owner lookup per VM, not per TAP — each is a meta transaction holding the namespace lock.
			orphans := make(map[string][]netlink.Link, len(prefixes))
			for _, l := range links {
				if prefix, ok := parseTAPName(tapPrefix, l.Attrs().Name); ok && slices.Contains(prefixes, prefix) {
					orphans[prefix] = append(orphans[prefix], l)
				}
			}
			var errs []error
			for _, prefix := range prefixes {
				inUse, err := vmInUse(ctx, prefix)
				if err != nil {
					errs = append(errs, fmt.Errorf("owner of TAP prefix %s: %w", prefix, err))
					continue
				}
				if inUse {
					continue
				}
				for _, l := range orphans[prefix] {
					name := l.Attrs().Name
					if err := deleteLinkFn(l); err != nil {
						logger.Warnf(ctx, "delete orphan TAP %s: %v", name, err)
					} else {
						logger.Infof(ctx, "collected id=%s iface=%s reason=orphan-tap", prefix, name)
					}
				}
			}
			return errors.Join(errs...)
		},
	}
}

// parseTAPName extracts the vmID prefix from a bridge TAP name "<tapPrefix><vmid8>-<nic>".
func parseTAPName(tapPrefix, name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, tapPrefix)
	if !ok {
		return "", false
	}
	prefix, _, ok := strings.CutLast(rest, "-")
	if !ok || prefix == "" {
		return "", false
	}
	return prefix, true
}
