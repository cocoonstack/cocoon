package cni

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
)

type cniSnapshot struct {
	dbVMIDs    map[string]struct{} // unique VM IDs from CNI DB records
	netnsNames []string            // VM IDs extracted from /var/run/netns/cocoon-*
}

// GCModule returns the GC module for orphan netns and stale CNI record cleanup.
func (c *CNI) GCModule() gc.Module[cniSnapshot] {
	return gc.Module[cniSnapshot]{
		Name:   typ,
		Locker: c.locker,
		ReadDB: func(_ context.Context) (cniSnapshot, error) {
			var snap cniSnapshot
			snap.dbVMIDs = make(map[string]struct{})
			if err := c.store.ReadRaw(func(idx *networkIndex) error {
				for _, rec := range idx.Networks {
					if rec != nil {
						snap.dbVMIDs[rec.VMID] = struct{}{}
					}
				}
				return nil
			}); err != nil {
				return snap, err
			}
			if entries, readErr := os.ReadDir(netnsBasePath); readErr == nil {
				for _, e := range entries {
					if name, ok := strings.CutPrefix(e.Name(), netnsPrefix); ok {
						snap.netnsNames = append(snap.netnsNames, name)
					}
				}
			}
			return snap, nil
		},
		Resolve: func(_ context.Context, snap cniSnapshot, others map[string]any) []string {
			active := gc.Collect(others, gc.VMIDs)
			candidates := maps.Clone(snap.dbVMIDs)
			for _, name := range snap.netnsNames {
				candidates[name] = struct{}{}
			}
			var orphans []string
			for id := range candidates {
				if _, ok := active[id]; !ok {
					orphans = append(orphans, id)
				}
			}
			slices.Sort(orphans)
			return orphans
		},
		Collect: func(ctx context.Context, ids []string, _ cniSnapshot) error {
			logger := log.WithFunc("gc.cni")
			var errs []error
			for _, vmID := range ids {
				// Lockless — orchestrator holds flock.
				var records []networkRecord
				if readErr := c.store.ReadRaw(func(idx *networkIndex) error {
					records = idx.byVMID(vmID)
					return nil
				}); readErr != nil {
					errs = append(errs, fmt.Errorf("read records for %s: %w", vmID, readErr))
					continue
				}

				// CNI DEL per NIC — a failed release keeps its record for the next GC cycle.
				downIDs, _ := c.tearDownNICs(ctx, vmID, netnsPath(vmID), records, false)

				nsName := netnsName(vmID)
				if err := deleteNetnsFn(ctx, nsName); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, fmt.Errorf("remove netns %s: %w", nsName, err))
					continue
				}

				// Lockless write.
				if len(downIDs) > 0 {
					if err := c.store.WriteRaw(func(idx *networkIndex) error {
						for _, id := range downIDs {
							delete(idx.Networks, id)
						}
						return nil
					}); err != nil {
						errs = append(errs, fmt.Errorf("clean DB for %s: %w", vmID, err))
						continue
					}
				}
				logger.Infof(ctx, "collected id=%s netns=%s nics=%d/%d reason=orphan",
					vmID, nsName, len(downIDs), len(records))
			}
			return errors.Join(errs...)
		},
	}
}

// RegisterGC registers the CNI GC module with the given Orchestrator.
func (c *CNI) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, c.GCModule())
}
