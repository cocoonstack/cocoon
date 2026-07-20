package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
)

const (
	tableRecords    = "records"
	tableTombstones = "tombstones"
)

// Store is one image namespace on the shared meta engine, presenting the
// legacy whole-index closure shape over record primitives. Image lookups are
// whole-map by nature (ref normalization, digest-prefix matching), so P0
// materializes per operation — the same IO profile as the legacy store; the
// sqlite schema splits refs into indexed tables in P2.
type Store[E any] struct {
	meta *metajson.Store
	ns   string
}

// NewMetaStore binds an image namespace on the shared engine.
func NewMetaStore[E any](store *metajson.Store, ns string) *Store[E] {
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

// Publish is Update for the publish critical sections — OCI pull,
// importTarLayers, importTarFromReader, cloudimg commit — whose closures move
// blob files inside the transaction (P0 adapter allowlist; the json engine
// runs closures exactly once).
func (s *Store[E]) Publish(ctx context.Context, fn func(*Index[E]) error) error {
	return s.meta.ImpureUpdate(ctx, s.ns, func(w meta.Writer) error {
		return s.applyDiff(ctx, w, fn)
	})
}

// LockedRead reads while the GC orchestrator holds the namespace lock.
func (s *Store[E]) LockedRead(ctx context.Context, fn func(*Index[E]) error) error {
	return s.meta.RawView(ctx, s.ns, func(r meta.Reader) error {
		idx, _, err := s.materialize(ctx, r)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

// LockedWrite writes while the GC orchestrator holds the namespace lock.
func (s *Store[E]) LockedWrite(ctx context.Context, fn func(*Index[E]) error) error {
	return s.meta.LockedUpdate(ctx, s.ns, func(w meta.Writer) error {
		return s.applyDiff(ctx, w, fn)
	})
}

// Locker exposes the namespace lock for gc.Module (P0 adapter).
func (s *Store[E]) Locker() (lock.Locker, error) {
	return s.meta.NamespaceLocker(s.ns)
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

// IndexCodec reproduces the legacy {"images":{...}} file byte-for-byte;
// records cross as raw messages (no per-record re-marshal).
type IndexCodec[E any] struct{}

// rawImageIndex mirrors Index's field layout with pass-through record bytes.
type rawImageIndex struct {
	Images     map[string]json.RawMessage `json:"images"`
	Tombstones map[string]json.RawMessage `json:"tombstones,omitempty"`
}

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
