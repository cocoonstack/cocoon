package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// VMGCSnapshot is the ReadDB-phase data for any hypervisor GC module (CH + FC share the shape).
type VMGCSnapshot struct {
	blobIDs     map[string]struct{}
	vmIDs       map[string]struct{}
	staleCreate []string
	runDirs     []string
	logDirs     []string
	reasons     map[string]string
}

func (s VMGCSnapshot) UsedBlobIDs() map[string]struct{} { return s.blobIDs }

func (s VMGCSnapshot) ActiveVMIDs() map[string]struct{} { return s.vmIDs }

// BuildGCModule builds GC module that scans DB and dirs for orphan VMs.
func (b *Backend) BuildGCModule() gc.Module[VMGCSnapshot] {
	return gc.Module[VMGCSnapshot]{
		Name:   b.Typ,
		Locker: b.Locker,
		ReadDB: func(_ context.Context) (VMGCSnapshot, error) {
			snap := VMGCSnapshot{reasons: make(map[string]string)}
			cutoff := time.Now().Add(-CreatingStateGCGrace)
			if err := b.DB.ReadRaw(func(idx *VMIndex) error {
				snap.blobIDs = make(map[string]struct{}, len(idx.VMs))
				snap.vmIDs = make(map[string]struct{}, len(idx.VMs))
				for id, rec := range idx.VMs {
					if rec == nil {
						continue
					}
					snap.vmIDs[id] = struct{}{}
					maps.Copy(snap.blobIDs, rec.ImageBlobIDs)
					if rec.State == types.VMStateCreating && rec.UpdatedAt.Before(cutoff) {
						snap.staleCreate = append(snap.staleCreate, id)
					}
				}
				return nil
			}); err != nil {
				return snap, err
			}
			var err error
			if snap.runDirs, err = utils.ScanSubdirs(b.Conf.RunDir()); err != nil {
				return snap, err
			}
			if snap.logDirs, err = utils.ScanSubdirs(b.Conf.LogDir()); err != nil {
				return snap, err
			}
			return snap, nil
		},
		Resolve: func(_ context.Context, snap VMGCSnapshot, _ map[string]any) []string {
			// "db" holds vms.json/vms.lock — exclude from orphan scan when RootDir == RunDir.
			reserved := map[string]struct{}{"db": {}}
			runOrphans := utils.FilterUnreferenced(snap.runDirs, snap.vmIDs, reserved)
			logOrphans := utils.FilterUnreferenced(snap.logDirs, snap.vmIDs, reserved)
			for _, id := range snap.staleCreate {
				snap.reasons[id] = "stale-creating"
			}
			for _, id := range runOrphans {
				if _, ok := snap.reasons[id]; !ok {
					snap.reasons[id] = "orphan-runDir"
				}
			}
			for _, id := range logOrphans {
				if _, ok := snap.reasons[id]; !ok {
					snap.reasons[id] = "orphan-logDir"
				}
			}
			candidates := slices.Concat(runOrphans, logOrphans, snap.staleCreate)
			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		Collect: func(ctx context.Context, ids []string, snap VMGCSnapshot) error {
			return b.gcCollect(ctx, ids, snap)
		},
	}
}

func (b *Backend) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, b.BuildGCModule())
}

// WatchPath returns VM index file path for filesystem-based watching.
func (b *Backend) WatchPath() string {
	return b.Conf.IndexFile()
}

// gcCollect kills leftover hypervisor processes, removes orphan dirs/records, and sweeps stale capture/staging leftovers under the orchestrator's flock.
func (b *Backend) gcCollect(ctx context.Context, ids []string, snap VMGCSnapshot) error {
	logger := log.WithFunc("gc." + b.Typ)
	errs := b.sweepStaleCaptureDirs(ctx, snap.runDirs)
	for _, id := range ids {
		runDir, logDir := b.Conf.VMRunDir(id), b.Conf.VMLogDir(id)
		_ = b.DB.ReadRaw(func(idx *VMIndex) error {
			if rec := idx.VMs[id]; rec != nil {
				runDir, logDir = rec.RunDir, rec.LogDir
			}
			return nil
		})
		b.killOrphanProcess(ctx, runDir)
		if err := RemoveVMDirs(runDir, logDir); err != nil {
			errs = append(errs, fmt.Errorf("remove vm %s: %w", id, err))
			continue
		}
		logger.Infof(ctx, "collected id=%s runDir=%s logDir=%s reason=%s",
			id, runDir, logDir, snap.reasons[id])
	}
	if err := b.CleanStalePlaceholders(ctx, ids); err != nil {
		errs = append(errs, fmt.Errorf("clean stale placeholders: %w", err))
	}
	return errors.Join(errs...)
}

// sweepStaleCaptureDirs removes crashed snapshot-*/.restore-staging leftovers inside every run dir once past the creating-grace age; an in-flight capture keeps its dir young (files land continuously), and restores finish long before the cutoff.
func (b *Backend) sweepStaleCaptureDirs(ctx context.Context, runDirNames []string) []error {
	cutoff := time.Now().Add(-CreatingStateGCGrace)
	var errs []error
	for _, name := range runDirNames {
		errs = append(errs, utils.RemoveMatching(ctx, filepath.Join(b.Conf.RunDir(), name), func(e os.DirEntry) bool {
			if !e.IsDir() || (!strings.HasPrefix(e.Name(), captureDirPrefix) && e.Name() != restoreStagingName) {
				return false
			}
			info, err := e.Info()
			return err == nil && info.ModTime().Before(cutoff)
		})...)
	}
	return errs
}

// killOrphanProcess terminates a leftover hypervisor process if PID matches the binary.
func (b *Backend) killOrphanProcess(ctx context.Context, runDir string) {
	pid, err := utils.ReadPIDFile(b.PIDFilePath(runDir))
	if err != nil {
		return
	}
	sockPath := SocketPath(runDir)
	if !utils.VerifyProcessCmdline(pid, b.Conf.BinaryName(), sockPath) {
		return
	}
	_ = utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, b.Conf.TerminateGracePeriod())
}
