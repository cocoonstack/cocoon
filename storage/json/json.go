package json

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

var _ storage.Store[struct{}] = (*Store[struct{}])(nil)

type Store[T any] struct {
	filePath string
	locker   lock.Locker
}

// New creates a JSON file-backed store with the given lock.
func New[T any](filePath string, locker lock.Locker) *Store[T] {
	return &Store[T]{filePath: filePath, locker: locker}
}

// ReadRaw loads the JSON file and passes the decoded data to fn without locking.
func (s *Store[T]) ReadRaw(fn func(*T) error) error {
	data, err := s.load()
	if err != nil {
		return err
	}
	return fn(data)
}

// WriteRaw loads, mutates via fn, and atomically writes back without locking.
func (s *Store[T]) WriteRaw(fn func(*T) error) error {
	data, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(data); err != nil {
		return err
	}
	if err := utils.AtomicWriteJSON(s.filePath, data); err != nil {
		return err
	}
	return nil
}

// With acquires the lock, loads the data, and passes it to fn read-only.
func (s *Store[T]) With(ctx context.Context, fn func(*T) error) error {
	return s.withLocked(ctx, func() error { return s.ReadRaw(fn) })
}

// Update acquires the lock, loads, mutates via fn, and atomically writes back.
func (s *Store[T]) Update(ctx context.Context, fn func(*T) error) error {
	return s.withLocked(ctx, func() error { return s.WriteRaw(fn) })
}

// TryLock attempts to acquire the store lock without blocking.
func (s *Store[T]) TryLock(ctx context.Context) (bool, error) {
	return s.locker.TryLock(ctx)
}

// Unlock releases the store lock.
func (s *Store[T]) Unlock(ctx context.Context) error {
	return s.locker.Unlock(ctx)
}

func (s *Store[T]) load() (*T, error) {
	var data T
	raw, err := os.ReadFile(s.filePath) //nolint:gosec
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			initData(&data)
			return &data, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.filePath, err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.filePath, err)
	}
	initData(&data)
	return &data, nil
}

func (s *Store[T]) withLocked(ctx context.Context, fn func() error) error {
	if err := s.locker.Lock(ctx); err != nil {
		return err
	}
	defer func() {
		if err := s.locker.Unlock(ctx); err != nil {
			log.WithFunc("storage.json").Warnf(ctx, "unlock %s: %v", s.filePath, err)
		}
	}()
	return fn()
}

func initData[T any](data *T) {
	if initer, ok := any(data).(storage.Initer); ok {
		initer.Init()
	}
}
