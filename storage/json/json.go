package json

import (
	"context"
	"errors"
	"io/fs"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

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
	data, err := s.load()
	if err != nil {
		return err
	}
	return fn(data)
}

// WriteRaw loads, mutates, atomically writes back — unlocked.
func (s *Store[T]) WriteRaw(fn func(*T) error) error {
	data, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(data); err != nil {
		return err
	}
	return utils.AtomicWriteJSON(s.filePath, data, utils.Sync)
}

// With runs fn read-only under the store lock.
func (s *Store[T]) With(ctx context.Context, fn func(*T) error) error {
	return s.withLocked(ctx, func() error { return s.ReadRaw(fn) })
}

// Update runs fn read-modify-write under the store lock.
func (s *Store[T]) Update(ctx context.Context, fn func(*T) error) error {
	return s.withLocked(ctx, func() error { return s.WriteRaw(fn) })
}

func (s *Store[T]) TryLock(ctx context.Context) (bool, error) {
	return s.locker.TryLock(ctx)
}

func (s *Store[T]) Unlock(ctx context.Context) error {
	return s.locker.Unlock(ctx)
}

func (s *Store[T]) load() (*T, error) {
	var data T
	if err := utils.ReadJSONFile(s.filePath, &data); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
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
