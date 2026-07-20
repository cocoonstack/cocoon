package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	gofrsflock "github.com/gofrs/flock"

	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
)

const (
	tableRecords    = "records"
	tableTombstones = "tombstones"
)

// Store is one image namespace on the shared meta engine, presenting the
// legacy whole-index closure shape over record primitives: image lookups are
// whole-map by nature (ref normalization, digest-prefix matching).
type Store[E any] struct {
	meta meta.Store
	ns   string
}

// NewMetaStore binds an image namespace on the shared engine.
func NewMetaStore[E any](store meta.Store, ns string) *Store[E] {
	return &Store[E]{meta: store, ns: ns}
}

// View runs fn over a materialized read snapshot.
func (s *Store[E]) View(ctx context.Context, fn func(*Index[E]) error) error {
	return s.meta.View(ctx, []string{s.ns}, func(r meta.Reader) error {
		idx, _, err := s.materialize(ctx, r)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

// Update runs a pure fn against the materialized index and writes the
// difference back as record operations.
func (s *Store[E]) Update(ctx context.Context, fn func(*Index[E]) error) error {
	return s.meta.Update(ctx, meta.Scope{Write: s.ns}, meta.CommitDurable, func(w meta.Writer) error {
		return s.applyDiff(ctx, w, fn)
	})
}

func (s *Store[E]) applyDiff(ctx context.Context, w meta.Writer, fn func(*Index[E]) error) error {
	idx, before, err := s.materialize(ctx, w)
	if err != nil {
		return err
	}
	if err := fn(idx); err != nil {
		return err
	}
	c := s.collection()
	for id := range before {
		if idx.Images[id] == nil {
			if err := c.Delete(ctx, w, id); err != nil {
				return err
			}
		}
	}
	for id, entry := range idx.Images {
		if entry == nil {
			continue
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode image entry %q: %w", id, err)
		}
		if prev, ok := before[id]; ok && string(prev) == string(raw) {
			continue
		}
		if err := upsert(ctx, w, c, id, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store[E]) materialize(ctx context.Context, r meta.Reader) (*Index[E], map[string]json.RawMessage, error) {
	idx := &Index[E]{}
	idx.Init()
	before := map[string]json.RawMessage{}
	if err := r.ScanRaw(ctx, s.ns, tableRecords, func(id string, raw json.RawMessage) error {
		var e E
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("decode image entry %q: %w", id, err)
		}
		idx.Images[id] = &e
		before[id] = slices.Clone(raw)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return idx, before, nil
}

func (s *Store[E]) collection() *meta.Collection[E] {
	return meta.NewCollection[E](s.meta, s.ns, tableRecords)
}

func upsert[E any](ctx context.Context, w meta.Writer, c *meta.Collection[E], id string, entry *E) error {
	err := c.Replace(ctx, w, id, entry)
	if errors.Is(err, meta.ErrNotFound) {
		return c.Insert(ctx, w, id, entry)
	}
	return err
}

// MetaNamespace declares an image namespace over its legacy images.json.
func MetaNamespace[E any](ns, indexFile, lockPath string) metajson.Namespace {
	return metajson.Namespace{Name: ns, FilePath: indexFile, LockPath: lockPath, Codec: IndexCodec[E]{}}
}

// rawImageIndex mirrors Index's field layout with pass-through record bytes.
type rawImageIndex struct {
	Images     map[string]json.RawMessage `json:"images"`
	Tombstones map[string]json.RawMessage `json:"tombstones,omitempty"`
}

// IndexCodec reproduces the legacy {"images":{...}} file byte-for-byte;
// records cross as raw messages (no per-record re-marshal).
type IndexCodec[E any] struct{}

func (IndexCodec[E]) Decode(data []byte) (*metajson.Model, error) {
	m := metajson.NewModel()
	if data == nil {
		return m, nil
	}
	var idx rawImageIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(idx.Images)) {
		m.Put(tableRecords, id, idx.Images[id])
	}
	for _, id := range slices.Sorted(maps.Keys(idx.Tombstones)) {
		m.Put(tableTombstones, id, idx.Tombstones[id])
	}
	return m, nil
}

func (IndexCodec[E]) Encode(m *metajson.Model) ([]byte, error) {
	buf := append([]byte(nil), `{"images":`...)
	buf, err := metajson.AppendRawMap(buf, metajson.CollectTable(m, tableRecords))
	if err != nil {
		return nil, err
	}
	if ts := metajson.CollectTable(m, tableTombstones); len(ts) > 0 {
		buf = append(buf, `,"tombstones":`...)
		if buf, err = metajson.AppendRawMap(buf, ts); err != nil {
			return nil, err
		}
	}
	return append(buf, '}', '\n'), nil
}

// BlobLocks holds per-digest blob locks for a publish critical section:
// sorted acquisition so concurrent multi-digest publishes cannot deadlock;
// Release never removes the lock files (flock synchronizes on the inode).
type BlobLocks struct {
	held []*gofrsflock.Flock
}

// Lock acquires every path in sorted order, blocking.
func (b *BlobLocks) Lock(paths ...string) error {
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	for _, p := range sorted {
		fl := gofrsflock.New(p)
		if err := fl.Lock(); err != nil {
			_ = fl.Close()
			b.Release()
			return fmt.Errorf("lock blob %s: %w", p, err)
		}
		b.held = append(b.held, fl)
	}
	return nil
}

// Release drops every held lock.
func (b *BlobLocks) Release() {
	for _, fl := range b.held {
		_ = fl.Close()
	}
	b.held = nil
}

// PinBlobs takes blobIDs' digest locks and verifies each blob file still
// exists: a re-pinning flow holds the lock while committing its pin (design
// §5), or image GC collects the blob between resolve and reserve.
func PinBlobs(cfg *BaseConfig, blobIDs map[string]struct{}) (func(), error) {
	if len(blobIDs) == 0 {
		return func() {}, nil
	}
	paths := make([]string, 0, len(blobIDs))
	for hex := range blobIDs {
		paths = append(paths, cfg.BlobLockPath(hex))
	}
	locks := &BlobLocks{}
	if err := locks.Lock(paths...); err != nil {
		return nil, err
	}
	for hex := range blobIDs {
		if _, err := os.Stat(cfg.BlobPath(hex)); err != nil {
			locks.Release()
			return nil, fmt.Errorf("image blob %s no longer on disk (collected before the pin): %w", hex, err)
		}
	}
	return locks.Release, nil
}
