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

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

var gcReservedDirNames = map[string]struct{}{"db": {}, CloneLocksDirName: {}}

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

func (s VMGCSnapshot) sweepDirs(runRoot string) []string {
	dirs := make([]string, 0, len(s.runDirs))
	for _, name := range s.runDirs {
		if _, ok := gcReservedDirNames[name]; !ok {
			dirs = append(dirs, filepath.Join(runRoot, name))
		}
	}
	return dirs
}

func (b *Backend) BuildGCModule() gc.Module[VMGCSnapshot] {
	return gc.Module[VMGCSnapshot]{
		Name:    b.Typ,
		Recover: b.gcRecover,
		ReadDB: func(ctx context.Context) (VMGCSnapshot, error) {
			snap := VMGCSnapshot{reasons: make(map[string]string)}
			if err := b.view(ctx, func(t *vmTx) error {
				snap.blobIDs = make(map[string]struct{})
				snap.vmIDs = make(map[string]struct{})
				return t.Scan(func(id string, rec *VMRecord) error {
					snap.vmIDs[id] = struct{}{}
					maps.Copy(snap.blobIDs, rec.ImageBlobIDs)
					if rec.State == types.VMStateCreating {
						snap.staleCreate = append(snap.staleCreate, id)
					}
					return nil
				})
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
			runOrphans := utils.FilterUnreferenced(snap.runDirs, snap.vmIDs, gcReservedDirNames)
			logOrphans := utils.FilterUnreferenced(snap.logDirs, snap.vmIDs, gcReservedDirNames)
			setReason := func(ids []string, reason string) {
				for _, id := range ids {
					if _, ok := snap.reasons[id]; !ok {
						snap.reasons[id] = reason
					}
				}
			}
			setReason(snap.staleCreate, "stale-creating")
			setReason(runOrphans, "orphan-runDir")
			setReason(logOrphans, "orphan-logDir")
			candidates := slices.Concat(runOrphans, logOrphans, snap.staleCreate)
			slices.Sort(candidates)
			return slices.Compact(candidates)
		},
		Collect: b.gcCollect,
	}
}

func (b *Backend) RegisterGC(orch *gc.Orchestrator) {
	orch.Register(b.BuildGCModule())
}

func (b *Backend) gcRecover(ctx context.Context) []error {
	var ids []string
	if err := b.view(ctx, func(t *vmTx) error {
		var err error
		ids, err = b.tombstones().PendingIDs(ctx, t.r)
		return err
	}); err != nil {
		return []error{err}
	}
	var errs []error
	for _, id := range ids {
		b.withOpsTryLock(ctx, id, func() {
			if _, err := b.RecoverTombstone(ctx, id); err != nil {
				errs = append(errs, fmt.Errorf("recover %s: %w", id, err))
			}
		})
	}
	return errs
}

func (b *Backend) gcCollect(ctx context.Context, ids []string, snap VMGCSnapshot) error {
	logger := log.WithFunc("gc." + b.Typ)
	errs := b.sweepStaleCaptureDirs(ctx, snap.sweepDirs(b.Conf.RunDir()))
	errs = append(errs, b.sweepStaleCloneLocks(ctx)...)
	for _, id := range ids {
		// Hold the ops lock: records are scanned before dirs, so an in-flight create's dirs read as orphans.
		ok := b.withOpsTryLock(ctx, id, func() {
			rec, err := b.PeekRecord(ctx, id)
			if err != nil {
				errs = append(errs, err)
				return
			}
			if rec == nil {
				// Recordless orphan dirs converge without a tombstone: no metadata points at them.
				runDir, logDir := b.Conf.VMRunDir(id), b.Conf.VMLogDir(id)
				// Fail closed: deleting sockets/disks under a still-live VMM corrupts it.
				if err := b.ensureOrphanVMMDead(ctx, runDir); err != nil {
					errs = append(errs, fmt.Errorf("orphan vmm for %s: %w (dirs kept)", id, err))
					return
				}
				if err := RemoveVMDirs(runDir, logDir); err != nil {
					errs = append(errs, fmt.Errorf("remove vm %s: %w", id, err))
					return
				}
				logger.Infof(ctx, "collected id=%s reason=%s", id, snap.reasons[id])
				return
			}
			// Revalidate under the lock: the unlocked scan may have read a stale state.
			if rec.State != types.VMStateCreating {
				return
			}
			if err := b.collectStaleCreate(ctx, id, rec); err != nil {
				errs = append(errs, fmt.Errorf("collect %s: %w", id, err))
				return
			}
			logger.Infof(ctx, "collected id=%s reason=%s", id, snap.reasons[id])
		})
		if !ok {
			logger.Warnf(ctx, "skip %s: ops lock busy (in-flight operation)", id)
		}
	}
	return errors.Join(errs...)
}

// sweepStaleCaptureDirs locks per dir: the staging name is fixed, so a fresh restore could otherwise recreate the dir between the age check and the removal.
func (b *Backend) sweepStaleCaptureDirs(ctx context.Context, runDirs []string) []error {
	cutoff := timeNow().Add(-StaleArtifactGCGrace)
	var errs []error
	for _, dir := range runDirs {
		// The run dir's basename is its VM ID — the lock domain of everything inside it.
		b.withOpsTryLock(ctx, filepath.Base(dir), func() {
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

// sweepStaleCloneLocks reclaims crash-orphaned lock files past the grace age via TryLock+Unlock, not a bare remove, so a live waiter can't be split onto a fresh inode.
func (b *Backend) sweepStaleCloneLocks(ctx context.Context) []error {
	dir := filepath.Join(b.Conf.RunDir(), CloneLocksDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	logger := log.WithFunc("gc." + b.Typ)
	cutoff := timeNow().Add(-StaleArtifactGCGrace)
	var errs []error
	for _, e := range entries {
		info, infoErr := e.Info()
		if infoErr != nil || info.ModTime().After(cutoff) {
			continue
		}
		ok, reclaimErr := flock.ReclaimTransient(ctx, filepath.Join(dir, e.Name()))
		if reclaimErr != nil {
			errs = append(errs, reclaimErr)
			continue
		}
		if !ok {
			continue
		}
		logger.Infof(ctx, "collected lock=%s reason=stale-clone-lock", e.Name())
	}
	return errs
}

// withOpsTryLock runs fn under the VM ops lock and reports false when another operation holds it; GC only try-locks, so lock order cannot deadlock.
func (b *Backend) withOpsTryLock(ctx context.Context, vmID string, fn func()) bool {
	unlock, ok, err := b.TryLockVMOps(ctx, vmID)
	if err != nil || !ok {
		return false
	}
	defer unlock()
	fn()
	return true
}

// ensureOrphanVMMDead kills any VMM on runDir's socket; the /proc scan covers a missing pidfile.
func (b *Backend) ensureOrphanVMMDead(ctx context.Context, runDir string) error {
	sockPath := SocketPath(runDir)
	if pid, err := utils.ReadPIDFile(b.PIDFilePath(runDir)); err == nil {
		if termErr := utils.TerminateProcess(ctx, pid, sockPath, b.Conf.TerminateGracePeriod()); termErr != nil {
			return termErr
		}
	}
	pids, err := utils.FindVMMByCmdline(b.Conf.BinaryName(), sockPath)
	if err != nil {
		return fmt.Errorf("/proc scan: %w", err)
	}
	for _, pid := range pids {
		if termErr := utils.TerminateProcess(ctx, pid, sockPath, b.Conf.TerminateGracePeriod()); termErr != nil {
			return termErr
		}
	}
	return nil
}

// VMIDFromRunPath reports the VM owning a `<runRoot>/<vmID>/<file>` path, excluding the reserved infrastructure dirs.
func VMIDFromRunPath(runRoot, path string) (string, bool) {
	dir := filepath.Clean(filepath.Dir(path))
	if filepath.Dir(dir) != filepath.Clean(runRoot) {
		return "", false
	}
	id := filepath.Base(dir)
	if _, reserved := gcReservedDirNames[id]; reserved {
		return "", false
	}
	return id, true
}
