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

	"github.com/cocoonstack/cocoon/lock"
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
		return commitLocked(target, models[sc.Write])
	}); err != nil {
		return err
	}
	return syncCommitted(target, mode)
}

// Close stops the event notifier; namespace files need no teardown.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.events != nil {
		s.events.stop()
		s.events = nil
	}
	return nil
}

func (s *Store) resolve(nss []string) ([]*nsState, error) {
	sorted := slices.Sorted(slices.Values(nss))
	sorted = slices.Compact(sorted)
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

// withLocked holds every namespace flock (fixed global order: sorted names)
// for fn's duration. Unlock errors are logged, never joined: fn's mutation
// may already be durably committed, and callers treat an error as "not
// committed" and destructively roll back; a leaked flock instead fails the
// next Lock loudly.
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
		for i := len(states) - 1; i >= 0; i-- {
			if err := states[i].locker.Unlock(ctx); err != nil {
				logger.Errorf(ctx, err, "unlock %s", states[i].def.Name)
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
	locker lock.Locker
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
		return &loaded{model: m}, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", def.FilePath, code(err, meta.ErrIO))
	}
	m, decodeErr := def.Codec.Decode(raw)
	if decodeErr == nil {
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
	return &loaded{model: prev, recovered: true}, nil
}

// commitLocked runs under the namespace flock: rotate the durable previous
// generation via link+rename (so a .prev exists at every instant), then
// atomically rename the fresh bytes in with no fsync — the post-release
// fsyncs in syncCommitted make them durable.
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
	return nil
}

func crash(step string) error {
	if testCrashStep == nil {
		return nil
	}
	return testCrashStep(step)
}

type txReader struct {
	models map[string]*loaded
}

var _ meta.Reader = (*txReader)(nil)

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

type txWriter struct {
	txReader
	write string
	model *Model
	mode  meta.CommitMode
}

var _ meta.Writer = (*txWriter)(nil)

func (w *txWriter) PutRaw(_ context.Context, ns, table, id string, raw json.RawMessage, relaxedOK bool) error {
	if err := w.check(ns, relaxedOK); err != nil {
		return err
	}
	w.model.Put(table, id, slices.Clone(raw))
	return nil
}

func (w *txWriter) DeleteRaw(_ context.Context, ns, table, id string, relaxedOK bool) error {
	if err := w.check(ns, relaxedOK); err != nil {
		return err
	}
	w.model.Delete(table, id)
	return nil
}

func (w *txWriter) NextSeq(_ context.Context, ns, table string) (meta.Seq, error) {
	if err := w.check(ns, true); err != nil {
		return 0, err
	}
	return meta.Seq(w.model.NextSeq(table)), nil
}

func (w *txWriter) Mode() meta.CommitMode { return w.mode }

func (w *txWriter) check(ns string, relaxedOK bool) error {
	if ns != w.write {
		return fmt.Errorf("write %s: %w", ns, meta.ErrScope)
	}
	if w.mode == meta.CommitRelaxed && !relaxedOK {
		return fmt.Errorf("write %s: %w", ns, meta.ErrDurabilityContract)
	}
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
