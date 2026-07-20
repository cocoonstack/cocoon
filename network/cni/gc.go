package cni

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock/vmlock"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/utils"
)

type cniSnapshot struct {
	dbVMIDs    map[string]struct{} // unique VM IDs from CNI DB records
	netnsNames []string            // VM IDs extracted from /var/run/netns/cocoon-*
}

// GCModule returns the GC module for orphan netns and stale CNI record cleanup.
func (c *CNI) GCModule() gc.Module[cniSnapshot] {
	return gc.Module[cniSnapshot]{
		Name:    typ,
		Recover: c.gcRecover,
		ReadDB: func(ctx context.Context) (cniSnapshot, error) {
			var snap cniSnapshot
			snap.dbVMIDs = make(map[string]struct{})
			if err := c.view(ctx, func(t *netTx) error {
				return t.scan(func(_ string, rec *networkRecord) error {
					snap.dbVMIDs[rec.VMID] = struct{}{}
					return nil
				})
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
			return utils.FilterUnreferenced(slices.Sorted(maps.Keys(candidates)), active)
		},
		Collect: func(ctx context.Context, ids []string, _ cniSnapshot) error {
			logger := log.WithFunc("gc.cni")
			var errs []error
			for _, vmID := range ids {
				// The owning VM's lock covers network teardown (design §5);
				// a held lock means an in-flight lifecycle operation — skip.
				lk, lockErr := vmlock.New(c.conf.RootDir, vmID)
				if lockErr != nil {
					errs = append(errs, lockErr)
					continue
				}
				ok, lockErr := lk.TryLock(ctx)
				if lockErr != nil || !ok {
					logger.Warnf(ctx, "skip %s: vm lock busy", vmID)
					continue
				}
				tdErr := c.teardownProtocol(ctx, vmID, nil, false)
				_ = lk.Unlock(ctx)
				if tdErr != nil {
					// Keep the netns: the next cycle's retried DEL runs with its context intact.
					errs = append(errs, fmt.Errorf("nic release incomplete for %s, netns kept: %w", vmID, tdErr))
					continue
				}
				logger.Infof(ctx, "collected id=%s netns=%s reason=orphan", vmID, netnsName(vmID))
			}
			return errors.Join(errs...)
		},
	}
}

// RegisterGC registers the CNI GC module with the given Orchestrator.
func (c *CNI) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, c.GCModule())
}

// gcRecover resumes existing network tombstones by phase before discovery,
// each under its owning VM's lock (design §5 recovery-precedes-discovery).
func (c *CNI) gcRecover(ctx context.Context) []error {
	var ids []string
	if err := c.view(ctx, func(t *netTx) error {
		return c.tombstones().Scan(ctx, t.r, func(id string, _ *tombstone.Record) error {
			ids = append(ids, id)
			return nil
		})
	}); err != nil {
		return []error{err}
	}
	var errs []error
	for _, vmID := range ids {
		lk, err := vmlock.New(c.conf.RootDir, vmID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ok, err := lk.TryLock(ctx)
		if err != nil || !ok {
			continue
		}
		if err := c.recoverTombstone(ctx, vmID); err != nil {
			errs = append(errs, fmt.Errorf("recover %s: %w", vmID, err))
		}
		_ = lk.Unlock(ctx)
	}
	return errs
}
