package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/cocoonstack/cocoon/utils"
)

// exitQueue buffers observed exits so the poll loop never blocks behind a pass in flight.
const exitQueue = 64

// watchKey identifies a supervised VM across backends.
type watchKey struct {
	backend string
	vmID    string
}

// exitEvent reports that the watched process generation exited; gen fences it against a newer record transition.
type exitEvent struct {
	key watchKey
	gen uint64
	at  time.Time
}

type watchEntry struct {
	proc utils.ProcRef
	gen  uint64
	fd   int
}

// procWatcher turns VMM process exits into events. Where the platform has no
// process-exit notification the registry stays empty and the daemon's reconcile
// ticker is the only detector.
type procWatcher struct {
	exits chan exitEvent

	mu     sync.Mutex
	byKey  map[watchKey]watchEntry
	byFD   map[int]watchKey
	poll   *poller
	closed bool
}

func newProcWatcher() *procWatcher {
	return &procWatcher{
		exits: make(chan exitEvent, exitQueue),
		byKey: map[watchKey]watchEntry{},
		byFD:  map[int]watchKey{},
	}
}

func (w *procWatcher) start(ctx context.Context) error {
	p, err := newPoller()
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.poll = p
	w.mu.Unlock()
	go w.run(ctx)
	return nil
}

func (w *procWatcher) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for key, entry := range w.byKey {
		delete(w.byKey, key)
		delete(w.byFD, entry.fd)
		closeFD(entry.fd)
	}
	if w.poll != nil {
		_ = w.poll.close()
	}
}

// ensure watches proc for key, replacing any entry naming a different generation.
func (w *procWatcher) ensure(key watchKey, proc utils.ProcRef, gen uint64) {
	w.mu.Lock()
	if cur, ok := w.byKey[key]; ok {
		if cur.proc == proc && cur.gen == gen {
			w.mu.Unlock()
			return
		}
		w.evictLocked(key, cur.fd)
	}
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return
	}

	fd, err := openPidfd(proc.PID)
	if err != nil {
		return
	}
	// Re-verify after the open: a pid recycled in between would bind the wrong process.
	if again, err := utils.ProcRefOf(proc.PID); err != nil || again != proc {
		closeFD(fd)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, taken := w.byKey[key]; w.closed || taken || w.poll == nil {
		closeFD(fd)
		return
	}
	if err := w.poll.add(fd); err != nil {
		closeFD(fd)
		return
	}
	w.byKey[key] = watchEntry{proc: proc, gen: gen, fd: fd}
	w.byFD[fd] = key
}

func (w *procWatcher) drop(key watchKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry, ok := w.byKey[key]; ok {
		w.evictLocked(key, entry.fd)
	}
}

// dropAbsent releases watches for VMs the latest scan of one backend no longer lists.
func (w *procWatcher) dropAbsent(backend string, seen map[string]struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, entry := range w.byKey {
		if key.backend != backend {
			continue
		}
		if _, ok := seen[key.vmID]; !ok {
			w.evictLocked(key, entry.fd)
		}
	}
}

// pidOf reports the watched pid, or zero when the VM is not watched.
func (w *procWatcher) pidOf(key watchKey) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.byKey[key].proc.PID
}

func (w *procWatcher) run(ctx context.Context) {
	ready := make([]int, exitQueue)
	for ctx.Err() == nil {
		n, err := w.poll.wait(ready)
		if err != nil {
			return
		}
		for _, fd := range ready[:n] {
			w.fire(ctx, fd)
		}
	}
}

// fire evicts the entry before publishing, so a restart adopting the same key cannot collide with the exit still in flight.
func (w *procWatcher) fire(ctx context.Context, fd int) {
	w.mu.Lock()
	key, ok := w.byFD[fd]
	if !ok {
		w.mu.Unlock()
		return
	}
	gen := w.byKey[key].gen
	w.evictLocked(key, fd)
	w.mu.Unlock()

	select {
	case w.exits <- exitEvent{key: key, gen: gen, at: time.Now()}:
	case <-ctx.Done():
	}
}

func (w *procWatcher) evictLocked(key watchKey, fd int) {
	delete(w.byKey, key)
	delete(w.byFD, fd)
	if w.poll != nil {
		_ = w.poll.remove(fd)
	}
	closeFD(fd)
}
