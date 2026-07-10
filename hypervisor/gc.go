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
	recRunDirs  []string
	reasons     map[string]string
}

func (s VMGCSnapshot) UsedBlobIDs() map[string]struct{} { return s.blobIDs }

func (s VMGCSnapshot) ActiveVMIDs() map[string]struct{} { return s.vmIDs }

// sweepDirs joins the scanned run-dir names with every persisted record RunDir (a --run-dir migration leaves crash leftovers in old roots the config no longer names), deduplicated.
func (s VMGCSnapshot) sweepDirs(runRoot string) []string {
	dirs := make([]string, 0, len(s.runDirs)+len(s.recRunDirs))
	seen := make(map[string]struct{}, cap(dirs))
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if _, ok := seen[dir]; !ok {
			seen[dir] = struct{}{}
			dirs = append(dirs, dir)
		}
	}
	for _, name := range s.runDirs {
		add(filepath.Join(runRoot, name))
	}
	for _, dir := range s.recRunDirs {
		add(dir)
	}
	return dirs
}

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
					if rec.RunDir != "" {
						snap.recRunDirs = append(snap.recRunDirs, rec.RunDir)
					}
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
			// "db" holds vms.json/vms.lock (when RootDir == RunDir); clone-locks holds live FC clone flocks.
			reserved := map[string]struct{}{"db": {}, CloneLocksDirName: {}}
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
	errs := b.sweepStaleCaptureDirs(ctx, snap.sweepDirs(b.Conf.RunDir()))
	// Only fully-reclaimed ids lose their DB record: unrecording a skipped VM
	// would strand a live VMM/dirs with no owner and let network GC tear it down.
	safeToUnrecord := make([]string, 0, len(ids))
	for _, id := range ids {
		runDir, logDir := b.Conf.VMRunDir(id), b.Conf.VMLogDir(id)
		_ = b.DB.ReadRaw(func(idx *VMIndex) error {
			if rec := idx.VMs[id]; rec != nil {
				runDir, logDir = rec.RunDir, rec.LogDir
			}
			return nil
		})
		// Ops lock excludes in-flight owners: a create pre-locks and mkdirs the run
		// dir before its DB record lands, so an unlocked "orphan" may be seconds old.
		ok := b.withOpsTryLock(ctx, runDir, func() {
			// Fail closed: deleting sockets/disks under a still-live VMM corrupts it.
			if err := b.ensureOrphanVMMDead(ctx, runDir); err != nil {
				errs = append(errs, fmt.Errorf("orphan vmm for %s: %w (dirs kept)", id, err))
				return
			}
			if err := RemoveVMDirs(runDir, logDir); err != nil {
				errs = append(errs, fmt.Errorf("remove vm %s: %w", id, err))
				return
			}
			logger.Infof(ctx, "collected id=%s runDir=%s logDir=%s reason=%s",
				id, runDir, logDir, snap.reasons[id])
			safeToUnrecord = append(safeToUnrecord, id)
		})
		if !ok {
			logger.Warnf(ctx, "skip %s: ops lock busy (in-flight operation)", id)
		}
	}
	if err := b.CleanStalePlaceholders(ctx, safeToUnrecord); err != nil {
		errs = append(errs, fmt.Errorf("clean stale placeholders: %w", err))
	}
	return errors.Join(errs...)
}

// sweepStaleCaptureDirs removes crashed snapshot-*/.restore-staging leftovers inside every run dir once past the creating-grace age. It runs per-dir under the VM ops lock: the staging name is fixed, so without it a fresh restore could recreate the dir between the age check and the removal (ABA) and lose its staging mid-flight.
func (b *Backend) sweepStaleCaptureDirs(ctx context.Context, runDirs []string) []error {
	cutoff := time.Now().Add(-CreatingStateGCGrace)
	var errs []error
	for _, dir := range runDirs {
		b.withOpsTryLock(ctx, dir, func() {
			errs = append(errs, utils.RemoveMatching(ctx, dir, func(e os.DirEntry) bool {
				if !e.IsDir() || (!strings.HasPrefix(e.Name(), captureDirPrefix) && e.Name() != restoreStagingName) {
					return false
				}
				info, err := e.Info()
				return err == nil && info.ModTime().Before(cutoff)
			})...)
		})
	}
	return errs
}

// withOpsTryLock runs fn holding the VM ops lock, reporting false (fn skipped) when an in-flight operation owns it; GC never blocks on ops locks, so the reversed order vs. the ops-outer/index-inner convention cannot deadlock.
func (b *Backend) withOpsTryLock(ctx context.Context, runDir string, fn func()) bool {
	l, err := opsLock(runDir)
	if err != nil {
		return false
	}
	if ok, err := l.TryLock(ctx); err != nil || !ok {
		return false
	}
	defer func() { _ = l.Unlock(ctx) }()
	fn()
	return true
}

// ensureOrphanVMMDead terminates any VMM bound to runDir's socket and errors unless none survive; a missing pidfile is not proof of death, so it also scans /proc by socket path.
func (b *Backend) ensureOrphanVMMDead(ctx context.Context, runDir string) error {
	sockPath := SocketPath(runDir)
	if pid, err := utils.ReadPIDFile(b.PIDFilePath(runDir)); err == nil {
		if termErr := utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, b.Conf.TerminateGracePeriod()); termErr != nil {
			return termErr
		}
	}
	pids, err := utils.FindVMMByCmdline(b.Conf.BinaryName(), sockPath)
	if err != nil {
		return fmt.Errorf("/proc scan: %w", err)
	}
	for _, pid := range pids {
		if termErr := utils.TerminateProcess(ctx, pid, b.Conf.BinaryName(), sockPath, b.Conf.TerminateGracePeriod()); termErr != nil {
			return termErr
		}
	}
	return nil
}
