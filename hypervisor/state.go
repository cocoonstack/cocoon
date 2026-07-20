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
	now := time.Now()
	// Metering entries stage inside the closure and publish only on nil return,
	// so a retried closure cannot double-emit (meta contract clause 1).
	var stopped []metering.Entry
	if err := b.update(ctx, func(t *vmTx) error {
		staged := []metering.Entry{}
		for _, id := range ids {
			r, err := t.get(id)
			if err != nil {
				return err
			}
			if r == nil {
				continue
			}
			r.State = state
			r.UpdatedAt = now
			if state == types.VMStateStopped && hasOpenComputeInterval(r) {
				r.StoppedAt = &now
				staged = append(staged, b.makeEntry(metering.KindVMComputeStop, id, metering.ReasonStopUser, shapeFromConfig(r.Config), now))
			}
			if err := t.put(id, r); err != nil {
				return err
			}
		}
		stopped = staged
		return nil
	}); err != nil {
		return err
	}
	b.emitAll(ctx, stopped)
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
	now := time.Now()
	if err := b.update(ctx, func(t *vmTx) error {
		r, err := t.get(id)
		if err != nil || r == nil {
			return err
		}
		r.State = types.VMStateError
		r.Quarantine = reason
		r.UpdatedAt = now
		return t.put(id, r)
	}); err != nil {
		log.WithFunc(b.Typ+".QuarantineVM").Errorf(ctx, err, "quarantine VM %s", id)
	}
}

// BatchMarkStarted flips ids to VMStateRunning; entrants with an open compute interval are stale-running (close stop-crash, then open fresh).
func (b *Backend) BatchMarkStarted(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now()
	// Staged-then-published like UpdateStates: closure purity under retry.
	var emits []metering.Entry
	if err := b.update(ctx, func(t *vmTx) error {
		staged := []metering.Entry{}
		for _, id := range ids {
			r, err := t.get(id)
			if err != nil {
				return err
			}
			if r == nil {
				continue
			}
			shape := shapeFromConfig(r.Config)
			if hasOpenComputeInterval(r) {
				staged = append(staged, b.makeEntry(metering.KindVMComputeStop, id, metering.ReasonStopCrash, shape, now))
			}
			reason := bootOrRestartReason(r.FirstBooted)
			staged = append(staged, b.makeEntry(metering.KindVMComputeStart, id, reason, shape, now))
			r.State = types.VMStateRunning
			r.StartedAt = &now
			r.StoppedAt = nil
			r.UpdatedAt = now
			r.FirstBooted = true
			if err := t.put(id, r); err != nil {
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

// CleanStalePlaceholders removes "creating" records past GC grace period.
func (b *Backend) CleanStalePlaceholders(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-CreatingStateGCGrace)
	// Runs while the GC orchestrator holds the namespace lock (legacy WriteRaw).
	return b.lockedUpdate(ctx, func(t *vmTx) error {
		for _, id := range ids {
			r, err := t.get(id)
			if err != nil {
				return err
			}
			if r == nil || r.State != types.VMStateCreating || !r.UpdatedAt.Before(cutoff) {
				continue
			}
			if n := r.Config.Name; n != "" {
				if err := t.nameDel(n); err != nil {
					return err
				}
			}
			if err := t.del(id); err != nil {
				return err
			}
		}
		return nil
	})
}

// closeStaleComputeInterval emits stop-crash and writes StoppedAt; precondition: caller confirmed the process is dead. Self-healing if the record vanishes (concurrent rm) or was already closed: skip emit.
func (b *Backend) closeStaleComputeInterval(ctx context.Context, rec *VMRecord) {
	now := time.Now()
	closed := false
	if err := b.update(ctx, func(t *vmTx) error {
		closed = false
		r, err := t.get(rec.ID)
		if err != nil {
			return err
		}
		if r == nil || !hasOpenComputeInterval(r) {
			return nil
		}
		if r.State == types.VMStateRunning {
			r.State = types.VMStateStopped
		}
		r.StoppedAt = &now
		r.UpdatedAt = now
		closed = true
		return t.put(rec.ID, r)
	}); err != nil {
		log.WithFunc(b.Typ+".closeStaleComputeInterval").Warnf(ctx, "close interval for %s: %v", rec.ID, err)
		return
	}
	if !closed {
		return
	}
	b.Metering.Emit(ctx, b.makeEntry(metering.KindVMComputeStop, rec.ID, metering.ReasonStopCrash, shapeFromConfig(rec.Config), now))
}

// reconcileToRunning flips State→Running for a drifted record whose process is alive. With an open compute interval (Error after Running) the ledger already matches; without one (rare orphan: BatchMarkStarted's DB write failed after a successful launch) we emit a fresh compute.start so a later stop doesn't fire an unmatched compute.stop.
func (b *Backend) reconcileToRunning(ctx context.Context, id string) {
	now := time.Now()
	var (
		emit   bool
		shape  metering.Shape
		reason metering.Reason
	)
	if err := b.update(ctx, func(t *vmTx) error {
		emit = false
		r, err := t.get(id)
		if err != nil {
			return err
		}
		if r == nil || r.State == types.VMStateRunning {
			return nil
		}
		if hasOpenComputeInterval(r) {
			r.State = types.VMStateRunning
			r.StoppedAt = nil
			r.UpdatedAt = now
			return t.put(id, r)
		}
		emit = true
		shape = shapeFromConfig(r.Config)
		reason = bootOrRestartReason(r.FirstBooted)
		r.State = types.VMStateRunning
		r.StartedAt = &now
		r.StoppedAt = nil
		r.UpdatedAt = now
		r.FirstBooted = true
		return t.put(id, r)
	}); err != nil {
		log.WithFunc(b.Typ+".reconcileToRunning").Warnf(ctx, "flip %s to running: %v", id, err)
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

// bootOrRestartReason picks the metering reason for a VM's compute.start event.
func bootOrRestartReason(firstBooted bool) metering.Reason {
	if firstBooted {
		return metering.ReasonRestart
	}
	return metering.ReasonBoot
}
