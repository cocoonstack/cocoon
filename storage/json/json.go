package json

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

const prevSuffix = ".prev"

var _ storage.Store[struct{}] = (*Store[struct{}])(nil)

// Store is a lock-guarded JSON file holding one value of type T.
type Store[T any] struct {
	filePath string
	locker   lock.Locker
}

// New returns a Store backed by filePath and guarded by locker.
func New[T any](filePath string, locker lock.Locker) *Store[T] {
	return &Store[T]{filePath: filePath, locker: locker}
}

// ReadRaw loads the JSON file unlocked.
func (s *Store[T]) ReadRaw(fn func(*T) error) error {
	data, _, err := s.load()
	if err != nil {
		return err
	}
	return fn(data)
}

// WriteRaw loads, mutates, atomically writes back — unlocked.
func (s *Store[T]) WriteRaw(fn func(*T) error) error {
	return s.writeRaw(fn, utils.Sync)
}

// With runs fn read-only under the store lock.
func (s *Store[T]) With(ctx context.Context, fn func(*T) error) error {
	return s.withLocked(ctx, func() error { return s.ReadRaw(fn) })
}

// Update runs fn read-modify-write under the store lock; all fsyncs run after release so waiters never queue behind stable-storage flushes, and load falls back to the .prev generation if a crash tears the unsynced rename.
func (s *Store[T]) Update(ctx context.Context, fn func(*T) error) error {
	if err := s.UpdateNoDirSync(ctx, fn); err != nil {
		return err
	}
	return utils.SyncParentDir(filepath.Dir(s.filePath))
}

// UpdateNoDirSync is Update without the parent-dir fsync, for placeholder states GC re-derives after power loss.
func (s *Store[T]) UpdateNoDirSync(ctx context.Context, fn func(*T) error) error {
	if err := s.withLocked(ctx, func() error { return s.writeRaw(fn, utils.NoSync) }); err != nil {
		return err
	}
	// Main first so a .prev sync failure still leaves the caller's own generation durable; syncing .prev too closes the double-tear window when the previous writer died before its own post-release fsync, near-free on an already-durable inode.
	if err := utils.SyncFile(s.filePath); err != nil {
		return err
	}
	if err := utils.SyncFile(s.filePath + prevSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store[T]) TryLock(ctx context.Context) (bool, error) {
	return s.locker.TryLock(ctx)
}

func (s *Store[T]) Unlock(ctx context.Context) error {
	return s.locker.Unlock(ctx)
}

func (s *Store[T]) writeRaw(fn func(*T) error, sync utils.SyncMode) error {
	data, recovered, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(data); err != nil {
		return err
	}
	if sync == utils.Sync {
		return utils.AtomicWriteJSON(s.filePath, data)
	}
	// Keep the durable previous generation reachable: the caller fsyncs only after release, and a crash in between may tear the fresh file on filesystems without rename-over data ordering. A recovered main is the torn one — rotating it in would destroy the only good generation — and the rotation goes link+rename so a .prev exists at every instant.
	if !recovered {
		tmp := s.filePath + prevSuffix + ".tmp"
		if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		switch err := os.Link(s.filePath, tmp); {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		default:
			if err := os.Rename(tmp, s.filePath+prevSuffix); err != nil {
				return err
			}
		}
	}
	return utils.AtomicWriteJSONNoSync(s.filePath, data)
}

// load reads the store; recovered reports that main was undecodable and the .prev generation was served instead. Read errors fail closed — only a decode failure of successfully read bytes is eligible for fallback.
func (s *Store[T]) load() (*T, bool, error) {
	var data T
	recovered := false
	raw, err := os.ReadFile(s.filePath) //nolint:gosec
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, false, fmt.Errorf("read %s: %w", s.filePath, err)
	default:
		if decodeErr := stdjson.Unmarshal(raw, &data); decodeErr != nil {
			var prev T
			if prevErr := utils.ReadJSONFile(s.filePath+prevSuffix, &prev); prevErr != nil {
				return nil, false, errors.Join(fmt.Errorf("decode %s: %w", s.filePath, decodeErr), prevErr)
			}
			data = prev
			recovered = true
			log.WithFunc("storage.json.load").Warnf(context.Background(), "%s undecodable (%v); recovered the previous generation", s.filePath, decodeErr)
		}
	}
	initData(&data)
	return &data, recovered, nil
}

func (s *Store[T]) withLocked(ctx context.Context, fn func() error) error {
	if err := s.locker.Lock(ctx); err != nil {
		return err
	}
	defer func() {
		if err := s.locker.Unlock(ctx); err != nil {
			log.WithFunc("storage.json.withLocked").Errorf(ctx, err, "unlock %s", s.filePath)
		}
	}()
	return fn()
}

func initData[T any](data *T) {
	if initer, ok := any(data).(storage.Initer); ok {
		initer.Init()
	}
}
