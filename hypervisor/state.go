package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	socketProbeTimeout = 500 * time.Millisecond

	// persistTimeout bounds uncancelable bookkeeping writes (quarantine, rollback): losing them to Ctrl-C leaves half-cleaned state authoritative.
	persistTimeout = 30 * time.Second
)

// WithRunningVM calls fn if rec still points to a live VM process.
func (b *Backend) WithRunningVM(ctx context.Context, rec *VMRecord, fn func(pid int) error) error {
	logger := log.WithFunc(b.Typ + ".WithRunningVM")
	pid, pidErr := utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
	if pidErr != nil && !errors.Is(pidErr, fs.ErrNotExist) {
		logger.Warnf(ctx, "read PID file: %v", pidErr)
	}
	sockPath := SocketPath(rec.RunDir)
	if utils.VerifyProcessCmdline(pid, b.Conf.BinaryName(), sockPath) {
		return fn(pid)
	}
	// Covers pidfile/socket cleaned up before VMM exited. Fail-closed if scan errors so callers don't treat inconclusive state as ErrNotRunning.
	scanned, scanErr := utils.FindVMMByCmdline(b.Conf.BinaryName(), sockPath)
	if scanErr != nil {
		return fmt.Errorf("vm %s: pidfile-based check failed and /proc scan errored: %w (resolve the host issue and retry)", rec.ID, scanErr)
	}
	if len(scanned) == 0 {
		return ErrNotRunning
	}
	logger.Warnf(ctx, "VM %s recovered live pids %v via cmdline scan", rec.ID, scanned)
	return fn(scanned[0])
}

// IsAPISocketLive: (true,nil)=confirmed live; (false,nil)=ENOENT/ECONNREFUSED; (true,err)=fail-closed for unknown dial errors.
func (b *Backend) IsAPISocketLive(ctx context.Context, rec *VMRecord) (bool, error) {
	sock := SocketPath(rec.RunDir)
	dialCtx, cancel := context.WithTimeout(ctx, socketProbeTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", sock)
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return false, nil
	}
	return true, err
}

// WithPausedVM pauses, runs fn, resumes; eager resume on success promotes its error, deferred resume on fn-error only logs.
func (b *Backend) WithPausedVM(ctx context.Context, rec *VMRecord, pause, resume, fn func() error) error {
	return b.WithRunningVM(ctx, rec, func(_ int) error {
		if err := pause(); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		var resumed bool
		var resumeErr error
		logger := log.WithFunc(b.Typ + ".WithPausedVM")
		doResume := func() {
			if resumed {
				return
			}
			resumed = true
			resumeErr = resume()
			if resumeErr != nil {
				logger.Warnf(ctx, "resume VM %s: %v", rec.ID, resumeErr)
			}
		}
		defer doResume()
		if err := fn(); err != nil {
			return err
		}
		doResume()
		if resumeErr != nil {
			return fmt.Errorf("snapshot data captured but resume failed: %w", resumeErr)
		}
		return nil
	})
}

// UpdateStates flips ids to Stopped or Error and emits compute.stop on Running→Stopped (Error paths can't prove the process is dead so the interval stays open until a confirmed-dead helper closes it). To open a fresh interval, use BatchMarkStarted — UpdateStates intentionally rejects Running to avoid silent ledger drift.
func (b *Backend) UpdateStates(ctx context.Context, ids []string, state types.VMState) error {
	if len(ids) == 0 {
		return nil
	}
	if state == types.VMStateRunning {
		return fmt.Errorf("UpdateStates(Running) not allowed; use BatchMarkStarted")
	}
	now := timeNow()
	return b.batchUpdateVMs(ctx, ids, func(r *VMRecord, id string) []metering.Entry {
		if state != types.VMStateStopped {
			markTransition(r, state, types.TransitionError, now)
			return nil
		}
		markTransition(r, state, types.TransitionStopUser, now)
		r.QuiescePending = needsQuiesce(r)
		if !hasOpenComputeInterval(r) {
			return nil
		}
		r.StoppedAt = &now
		return []metering.Entry{b.makeEntry(metering.KindVMComputeStop, id, metering.ReasonStopUser, shapeFromConfig(r.Config), now)}
	})
}

// batchUpdateVMs mutates each present id in one transaction; mutate's
// metering entries stage inside the closure and publish only after commit,
// so a retried closure cannot double-emit (meta contract clause 1).
func (b *Backend) batchUpdateVMs(ctx context.Context, ids []string, mutate func(r *VMRecord, id string) []metering.Entry) error {
	var emits []metering.Entry
	if err := b.update(ctx, func(t *vmTx) error {
		staged := []metering.Entry{}
		for _, id := range ids {
			r, err := t.Get(id)
			if err != nil {
				return err
			}
			if r == nil {
				continue
			}
			staged = append(staged, mutate(r, id)...)
			if err := t.Put(id, r); err != nil {
				return err
			}
		}
		emits = staged
		return nil
	}); err != nil {
		return err
	}
	b.emitAll(ctx, emits)
	return nil
}

// MarkError flips a single VM's state to VMStateError, logging on persist failure.
func (b *Backend) MarkError(ctx context.Context, id string) {
	if err := b.UpdateStates(ctx, []string{id}, types.VMStateError); err != nil {
		log.WithFunc(b.Typ+".MarkError").Errorf(ctx, err, "mark VM %s error", id)
	}
}

// QuarantineVM marks the VM error and persists the quarantine reason (see VMRecord.Quarantine).
func (b *Backend) QuarantineVM(ctx context.Context, id, reason string) {
	ctx, cancel := detachedWrite(ctx)
	defer cancel()
	now := timeNow()
	if err := b.update(ctx, func(t *vmTx) error {
		r, err := t.Get(id)
		if err != nil || r == nil {
			return err
		}
		markTransition(r, types.VMStateError, types.TransitionError, now)
		r.Quarantine = reason
		return t.Put(id, r)
	}); err != nil {
		log.WithFunc(b.Typ+".QuarantineVM").Errorf(ctx, err, "quarantine VM %s", id)
	}
}

// BatchMarkStarted flips ids to VMStateRunning; entrants with an open compute interval are stale-running (close stop-crash, then open fresh).
func (b *Backend) BatchMarkStarted(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := timeNow()
	return b.batchUpdateVMs(ctx, ids, func(r *VMRecord, id string) []metering.Entry {
		shape := shapeFromConfig(r.Config)
		emitReason, transition := bootReasons(r.FirstBooted)
		var staged []metering.Entry
		if hasOpenComputeInterval(r) {
			staged = append(staged, b.makeEntry(metering.KindVMComputeStop, id, metering.ReasonStopCrash, shape, now))
		}
		staged = append(staged, b.makeEntry(metering.KindVMComputeStart, id, emitReason, shape, now))
		markTransition(r, types.VMStateRunning, transition, now)
		r.StartedAt = &now
		r.StoppedAt = nil
		r.FirstBooted = true
		r.QuiescePending = false
		return staged
	})
}

// ReconcileToRunning flips State→Running for a drifted record whose process is alive; the caller holds the VM ops lock. With an open compute interval (Error after Running) the ledger already matches; without one (rare orphan: BatchMarkStarted's DB write failed after a successful launch) we emit a fresh compute.start so a later stop doesn't fire an unmatched compute.stop.
func (b *Backend) ReconcileToRunning(ctx context.Context, id string) {
	now := timeNow()
	var (
		emit   bool
		shape  metering.Shape
		reason metering.Reason
	)
	if err := b.update(ctx, func(t *vmTx) error {
		emit = false
		r, err := t.Get(id)
		if err != nil {
			return err
		}
		if r == nil || r.State == types.VMStateRunning {
			return nil
		}
		emitReason, transition := bootReasons(r.FirstBooted)
		open := hasOpenComputeInterval(r)
		markTransition(r, types.VMStateRunning, transition, now)
		r.StoppedAt = nil
		r.QuiescePending = false
		if open {
			return t.Put(id, r)
		}
		emit = true
		shape = shapeFromConfig(r.Config)
		reason = emitReason
		r.StartedAt = &now
		r.FirstBooted = true
		return t.Put(id, r)
	}); err != nil {
		log.WithFunc(b.Typ+".ReconcileToRunning").Warnf(ctx, "flip %s to running: %v", id, err)
		return
	}
	if emit {
		b.Metering.Emit(ctx, b.makeEntry(metering.KindVMComputeStart, id, reason, shape, now))
	}
}

// detachedWrite returns a context for bookkeeping writes that must survive caller cancellation, bounded by persistTimeout.
func detachedWrite(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
}

// hasOpenComputeInterval reports whether the VM's record shows an unmatched compute.start (StoppedAt is the ledger-close sentinel; transitions to Running clear it).
func hasOpenComputeInterval(r *VMRecord) bool {
	return r != nil && r.StartedAt != nil && r.StoppedAt == nil
}

// markTransition stamps one committed state change; every record state write goes through it so generations stay dense enough to fence a stale observation against a newer transition.
func markTransition(r *VMRecord, state types.VMState, reason types.TransitionReason, at time.Time) {
	r.State = state
	r.TransitionGeneration++
	r.LastTransitionReason = reason
	r.LastTransitionAt = &at
	r.UpdatedAt = at
}

// needsQuiesce reports whether the VM owns host plumbing a stop must bring down; it mirrors the provider selection, where a missing backend or NIC set means there is nothing to quiesce.
func needsQuiesce(r *VMRecord) bool {
	return r != nil && r.ResolvedNetBackend() != "" && len(r.NetworkConfigs) > 0
}

// bootReasons maps first-boot state onto the metering and transition vocabularies, which name the same event.
func bootReasons(firstBooted bool) (metering.Reason, types.TransitionReason) {
	if firstBooted {
		return metering.ReasonRestart, types.TransitionRestart
	}
	return metering.ReasonBoot, types.TransitionBoot
}
