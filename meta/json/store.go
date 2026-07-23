package json

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/utils"
)

const prevSuffix = ".prev"

// testCrashStep aborts a commit right after the named write step when set;
// testWatchErrs injects a forced fsnotify overflow/watch error (§7 gate).
var (
	testCrashStep func(step string) error
	testWatchErrs chan struct{}
)

// Namespace declares one namespace's file, lock and format.
type Namespace struct {
	Name     string
	FilePath string
	LockPath string
	Codec    Codec
}

var _ meta.Store = (*Store)(nil)

// Store is the json engine: one flocked file per namespace, legacy write
// order (rotate .prev under lock, atomic rename, post-release fsyncs).
type Store struct {
	nss map[string]*nsState

	mu     sync.Mutex
	events *notifier
	closed bool
}

// Open validates the namespace set and returns a Store; it touches no files.
func Open(namespaces ...Namespace) (*Store, error) {
	s := &Store{nss: map[string]*nsState{}}
	for _, def := range namespaces {
		if def.Name == "" || def.FilePath == "" || def.LockPath == "" || def.Codec == nil {
			return nil, fmt.Errorf("namespace %q: incomplete definition", def.Name)
		}
		if _, ok := s.nss[def.Name]; ok {
			return nil, fmt.Errorf("namespace %q declared twice", def.Name)
		}
		s.nss[def.Name] = &nsState{def: def, locker: flock.New(def.LockPath)}
	}
	return s, nil
}

func (s *Store) View(ctx context.Context, nss []string, fn func(meta.Reader) error) error {
	states, err := s.resolve(nss)
	if err != nil {
		return err
	}
	return s.withLocked(ctx, states, func() error {
		models, err := s.loadAll(ctx, states)
		if err != nil {
			return err
		}
		return fn(&txReader{models: models})
	})
}

func (s *Store) Update(ctx context.Context, sc meta.Scope, mode meta.CommitMode, fn func(meta.Writer) error) error {
	if sc.Write == "" {
		return fmt.Errorf("update requires a write namespace: %w", meta.ErrScope)
	}
	states, err := s.resolve(append([]string{sc.Write}, sc.Read...))
	if err != nil {
		return err
	}
	target := s.nss[sc.Write]
	committed := false
	if err := s.withLocked(ctx, states, func() error {
		models, err := s.loadAll(ctx, states)
		if err != nil {
			return err
		}
		w := &txWriter{
			txReader: txReader{models: models},
			write:    sc.Write,
			model:    models[sc.Write].model,
			mode:     mode,
		}
		if err := fn(w); err != nil {
			return err
		}
		// A clean transaction commits nothing — the encode/rotate/fsync tail
		// would rewrite identical bytes on every read-only guard. A recovered
		// generation still commits: that is the read-repair of a torn main.
		if !models[sc.Write].model.Dirty() && !models[sc.Write].recovered {
			return nil
		}
		if err := commitLocked(target, models[sc.Write]); err != nil {
			return err
		}
		committed = true
		return nil
	}); err != nil {
		return err
	}
	if !committed {
		return nil
	}
	return syncCommitted(target, mode)
}

// Close stops the event notifier; namespace files need no teardown.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.events != nil {
		s.events.b.Stop()
		s.events = nil
	}
	return nil
}

func (s *Store) resolve(nss []string) ([]*nsState, error) {
	sorted := slices.Compact(slices.Sorted(slices.Values(nss)))
	states := make([]*nsState, 0, len(sorted))
	for _, name := range sorted {
		st, ok := s.nss[name]
		if !ok {
			return nil, fmt.Errorf("namespace %q: %w", name, meta.ErrScope)
		}
		states = append(states, st)
	}
	return states, nil
}

// withLocked holds every namespace flock (sorted names = fixed global order).
// Unlock errors log only: joining them would make callers roll back an
// already-durable commit; a leaked flock fails the next Lock loudly instead.
func (s *Store) withLocked(ctx context.Context, states []*nsState, fn func() error) error {
	logger := log.WithFunc("meta.json.withLocked")
	for i, st := range states {
		if err := st.locker.Lock(ctx); err != nil {
			for j := i - 1; j >= 0; j-- {
				if uerr := states[j].locker.Unlock(ctx); uerr != nil {
					logger.Errorf(ctx, uerr, "unlock %s", states[j].def.Name)
				}
			}
			return fmt.Errorf("lock %s: %w", st.def.Name, err)
		}
	}
	defer func() {
		for _, st := range slices.Backward(states) {
			if err := st.locker.Unlock(ctx); err != nil {
				logger.Errorf(ctx, err, "unlock %s", st.def.Name)
			}
		}
	}()
	return fn()
}

func (s *Store) loadAll(ctx context.Context, states []*nsState) (map[string]*loaded, error) {
	models := make(map[string]*loaded, len(states))
	for _, st := range states {
		l, err := loadNamespace(ctx, st.def)
		if err != nil {
			return nil, err
		}
		models[st.def.Name] = l
	}
	return models, nil
}

type nsState struct {
	def    Namespace
	locker *flock.Lock
}

type loaded struct {
	model *Model
	// recovered means main was undecodable and .prev was served; commit must
	// not rotate, or it would destroy the only good generation.
	recovered bool
}

// loadNamespace ports the legacy load: a missing file is empty, an
// undecodable main falls back to the .prev generation, read errors fail closed.
func loadNamespace(ctx context.Context, def Namespace) (*loaded, error) {
	raw, err := os.ReadFile(def.FilePath) //nolint:gosec
	switch {
	case errors.Is(err, fs.ErrNotExist):
		m, cerr := def.Codec.Decode(nil)
		if cerr != nil {
			return nil, cerr
		}
		m.markClean()
		return &loaded{model: m}, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", def.FilePath, code(err, meta.ErrIO))
	}
	m, decodeErr := def.Codec.Decode(raw)
	if decodeErr == nil {
		m.markClean()
		return &loaded{model: m}, nil
	}
	prevRaw, prevErr := os.ReadFile(def.FilePath + prevSuffix) //nolint:gosec
	if prevErr != nil {
		return nil, code(errors.Join(fmt.Errorf("decode %s: %w", def.FilePath, decodeErr), prevErr), meta.ErrCorrupt)
	}
	prev, prevDecodeErr := def.Codec.Decode(prevRaw)
	if prevDecodeErr != nil {
		return nil, code(errors.Join(fmt.Errorf("decode %s: %w", def.FilePath, decodeErr), prevDecodeErr), meta.ErrCorrupt)
	}
	log.WithFunc("meta.json.loadNamespace").Warnf(ctx, "%s undecodable (%v); recovered the previous generation", def.FilePath, decodeErr)
	prev.markClean()
	return &loaded{model: prev, recovered: true}, nil
}

// commitLocked rotates .prev via link+rename (one exists at every instant),
// then renames the fresh bytes in; syncCommitted makes them durable.
func commitLocked(st *nsState, l *loaded) error {
	data, err := st.def.Codec.Encode(l.model)
	if err != nil {
		return err
	}
	path := st.def.FilePath
	if !l.recovered {
		tmp := path + prevSuffix + ".tmp"
		if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return code(err, meta.ErrIO)
		}
		switch err := os.Link(path, tmp); {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return code(err, meta.ErrIO)
		default:
			if err := crash("prev-linked"); err != nil {
				return err
			}
			if err := os.Rename(tmp, path+prevSuffix); err != nil {
				return code(err, meta.ErrIO)
			}
		}
		if err := crash("prev-rotated"); err != nil {
			return err
		}
	}
	if err := utils.AtomicWriteFileNoSync(path, data, 0o644); err != nil {
		return code(err, meta.ErrIO)
	}
	return crash("main-renamed")
}

// syncCommitted runs after the flock is released: main first so a .prev sync
// failure still leaves the caller's own generation durable; the parent-dir
// sync is what CommitRelaxed relinquishes.
func syncCommitted(st *nsState, mode meta.CommitMode) error {
	path := st.def.FilePath
	if err := utils.SyncFile(path); err != nil {
		return code(err, meta.ErrIO)
	}
	if err := crash("main-synced"); err != nil {
		return err
	}
	if err := utils.SyncFile(path + prevSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return code(err, meta.ErrIO)
	}
	if mode == meta.CommitRelaxed {
		return nil
	}
	if err := crash("prev-synced"); err != nil {
		return err
	}
	if err := utils.SyncParentDir(filepath.Dir(path)); err != nil {
		return code(err, meta.ErrIO)
	}
	return crash("dir-synced")
}

func crash(step string) error {
	if testCrashStep == nil {
		return nil
	}
	return testCrashStep(step)
}

var _ meta.Reader = (*txReader)(nil)

type txReader struct {
	models map[string]*loaded
}

func (r *txReader) GetRaw(_ context.Context, ns, table, id string) (json.RawMessage, bool, error) {
	l, ok := r.models[ns]
	if !ok {
		return nil, false, fmt.Errorf("read %s: %w", ns, meta.ErrScope)
	}
	raw, ok := l.model.Get(table, id)
	// Detached values (contract clause 4): aliasing model bytes would let a
	// caller mutate committed state without PutRaw's scope/durability checks.
	return slices.Clone(raw), ok, nil
}

func (r *txReader) ScanRaw(_ context.Context, ns, table string, fn func(id string, raw json.RawMessage) error) error {
	l, ok := r.models[ns]
	if !ok {
		return fmt.Errorf("read %s: %w", ns, meta.ErrScope)
	}
	return l.model.Scan(table, func(id string, raw json.RawMessage) error {
		return fn(id, slices.Clone(raw))
	})
}

var _ meta.Writer = (*txWriter)(nil)

type txWriter struct {
	txReader
	write string
	model *Model
	mode  meta.CommitMode
}

func (w *txWriter) PutRaw(_ context.Context, ns, table, id string, raw json.RawMessage, relaxedOK bool) error {
	if err := meta.CheckWriteScope(ns, w.write, w.mode, relaxedOK); err != nil {
		return err
	}
	w.model.Put(table, id, slices.Clone(raw))
	return nil
}

func (w *txWriter) DeleteRaw(_ context.Context, ns, table, id string, relaxedOK bool) error {
	if err := meta.CheckWriteScope(ns, w.write, w.mode, relaxedOK); err != nil {
		return err
	}
	w.model.Delete(table, id)
	return nil
}

// coded pairs an engine error with its taxonomy sentinel without changing the message text.
type coded struct {
	err  error
	mark error
}

func (c *coded) Error() string   { return c.err.Error() }
func (c *coded) Unwrap() []error { return []error{c.err, c.mark} }

func code(err, mark error) error {
	if errors.Is(err, syscall.ENOSPC) {
		mark = meta.ErrNoSpace
	}
	return &coded{err: err, mark: mark}
}
