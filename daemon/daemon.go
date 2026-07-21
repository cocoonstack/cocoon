// Package daemon is the resident supervisor for VMs the cocoon CLI created: it adopts their
// VMM processes, observes exits, and converges metadata and host networking — optionally, so no
// CLI verb depends on it.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// DefaultReconcileInterval floors how often a full pass runs; meta events drive faster ones.
	DefaultReconcileInterval = 5 * time.Second

	// wakeDebounce coalesces event bursts: the json engine's View holds a cross-process namespace lock, so unthrottled passes stall every concurrent CLI invocation.
	wakeDebounce = 200 * time.Millisecond

	lockFileName = "daemon.lock"
)

// ErrAlreadyRunning reports another daemon instance holding the single-instance lock.
var ErrAlreadyRunning = errors.New("another cocoon daemon is already running")

// Supervisor is the backend surface supervision drives.
type Supervisor = hypervisor.Supervisable

// Config carries the daemon's runtime knobs and injected collaborators.
type Config struct {
	RootDir           string
	ReconcileInterval time.Duration
	// GCInterval enables the periodic GC run; zero (the default) leaves GC a CLI verb.
	GCInterval time.Duration
	GC         func(context.Context) error
	// APIAddr is the read-only API's unix socket path; empty disables it.
	APIAddr     string
	APISockMode uint32
}

// Daemon supervises every configured backend from one process.
type Daemon struct {
	conf      Config
	store     meta.Store
	backends  map[string]Supervisor
	order     []Supervisor
	watcher   *procWatcher
	gcRunning atomic.Bool
	state     *cache
}

// New builds a daemon over already-wired backends; store is the shared meta store the CLI also writes.
func New(conf Config, store meta.Store, backends []Supervisor) (*Daemon, error) {
	if conf.RootDir == "" {
		return nil, fmt.Errorf("root dir must not be empty")
	}
	if len(backends) == 0 {
		return nil, fmt.Errorf("no hypervisor backends to supervise")
	}
	if conf.ReconcileInterval <= 0 {
		conf.ReconcileInterval = DefaultReconcileInterval
	}
	byType := make(map[string]Supervisor, len(backends))
	for _, b := range backends {
		byType[b.Type()] = b
	}
	return &Daemon{
		conf:     conf,
		store:    store,
		backends: byType,
		order:    backends,
		watcher:  newProcWatcher(),
		state:    newCache(),
	}, nil
}

// Run supervises until ctx is canceled; it returns ErrAlreadyRunning when another instance holds the lock.
func (d *Daemon) Run(ctx context.Context) error {
	logger := log.WithFunc("daemon.Run")
	unlock, lockErr := d.lockInstance(ctx)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()

	if err := d.watcher.start(ctx); err != nil {
		return fmt.Errorf("start process watcher: %w", err)
	}
	defer d.watcher.close()

	if d.conf.APIAddr != "" {
		stopAPI, apiErr := d.serveAPI(ctx)
		if apiErr != nil {
			return fmt.Errorf("serve api: %w", apiErr)
		}
		defer stopAPI()
	}

	events, release, subErr := d.store.Events(ctx)
	if subErr != nil {
		logger.Warnf(ctx, "meta change events unavailable, reconciling on the ticker only: %v", subErr)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()

	ticker := time.NewTicker(d.conf.ReconcileInterval)
	defer ticker.Stop()
	gcTick, stopGC := d.gcTick()
	defer stopGC()

	logger.Infof(ctx, "supervising %d backend(s), reconcile every %s", len(d.order), d.conf.ReconcileInterval)
	d.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// A dead subscription is invisible — the meta layer never closes a subscriber channel — so the ticker doubles as the resubscribe cadence.
			if events == nil {
				if ch, rel, retryErr := d.store.Events(ctx); retryErr == nil {
					events, release = ch, rel
					logger.Info(ctx, "meta change events restored")
				}
			}
			d.reconcile(ctx)
		case <-events:
			if !d.settle(ctx) {
				return nil
			}
			drain(events)
			d.reconcile(ctx)
		case ev := <-d.watcher.exits:
			d.handleExit(ctx, ev)
			// The convergence just written wakes this daemon's own subscription; its pass already ran.
			drain(events)
		case <-gcTick:
			d.runGC(ctx)
		}
	}
}

func (d *Daemon) lockInstance(ctx context.Context) (func(), error) {
	dir := filepath.Join(d.conf.RootDir, "locks")
	if err := utils.EnsureDirs(dir); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(dir, lockFileName))
	ok, err := l.TryLock(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire daemon lock: %w", err)
	}
	if !ok {
		return nil, ErrAlreadyRunning
	}
	return func() { _ = l.Unlock(ctx) }, nil
}

// settle waits out the debounce window; false means the daemon is shutting down.
func (d *Daemon) settle(ctx context.Context) bool {
	timer := time.NewTimer(wakeDebounce)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// gcTick returns the periodic-GC channel and its stop. GC is off unless an
// interval is configured; a nil channel never fires, so the select arm goes inert.
func (d *Daemon) gcTick() (<-chan time.Time, func()) {
	if d.conf.GCInterval <= 0 || d.conf.GC == nil {
		return nil, func() {}
	}
	t := time.NewTicker(d.conf.GCInterval)
	return t.C, t.Stop
}

// runGC starts a collection off the supervision loop, unless one is still in flight.
func (d *Daemon) runGC(ctx context.Context) {
	if !d.gcRunning.CompareAndSwap(false, true) {
		log.WithFunc("daemon.runGC").Warn(ctx, "skip gc: previous run still active")
		return
	}
	go func() {
		defer d.gcRunning.Store(false)
		if err := d.conf.GC(ctx); err != nil {
			log.WithFunc("daemon.runGC").Error(ctx, err, "gc run")
		}
	}()
}

func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
