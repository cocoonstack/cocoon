package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const indexSep = "\x00"

// Option configures a Collection at construction.
type Option[R any] func(*Collection[R])

// WithUnique declares a persisted unique index; empty keys are sparse (unindexed).
func WithUnique[R any](name string, key func(*R) string) Option[R] {
	return func(c *Collection[R]) { c.uniques[name] = key }
}

// WithIndex declares a non-unique index served by scan-and-filter.
func WithIndex[R any](name string, key func(*R) string) Option[R] {
	return func(c *Collection[R]) { c.indexes[name] = key }
}

// Collection is a typed record set inside one namespace table. All reads
// hand back detached values; persisting a change requires Replace.
type Collection[R any] struct {
	store   Store
	ns      string
	table   string
	uniques map[string]func(*R) string
	indexes map[string]func(*R) string
}

// NewCollection binds a Collection to a (namespace, table) pair on s.
func NewCollection[R any](s Store, ns, table string, opts ...Option[R]) *Collection[R] {
	c := &Collection[R]{
		store:   s,
		ns:      ns,
		table:   table,
		uniques: map[string]func(*R) string{},
		indexes: map[string]func(*R) string{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Get returns a detached copy of id, or ErrNotFound.
func (c *Collection[R]) Get(ctx context.Context, r Reader, id string) (*R, error) {
	raw, ok, err := r.GetRaw(ctx, c.ns, c.table, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s/%s %q: %w", c.ns, c.table, id, ErrNotFound)
	}
	return c.decode(id, raw)
}

// Get1 is single-op sugar wrapping an auto View.
func (c *Collection[R]) Get1(ctx context.Context, id string) (rec *R, err error) {
	err = c.store.View(ctx, []string{c.ns}, func(r Reader) error {
		rec, err = c.Get(ctx, r, id)
		return err
	})
	return rec, err
}

// Insert adds a new record; an existing id or unique-index collision is ErrConflict.
func (c *Collection[R]) Insert(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error {
	if _, ok, err := w.GetRaw(ctx, c.ns, c.table, id); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s/%s %q exists: %w", c.ns, c.table, id, ErrConflict)
	}
	relaxed := relaxedOK(opts)
	if err := c.claimUniques(ctx, w, id, rec, relaxed); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode %s/%s %q: %w", c.ns, c.table, id, err)
	}
	return w.PutRaw(ctx, c.ns, c.table, id, raw, relaxed)
}

// Replace overwrites an existing record; an absent id is ErrNotFound.
func (c *Collection[R]) Replace(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error {
	old, err := c.Get(ctx, w, id)
	if err != nil {
		return err
	}
	relaxed := relaxedOK(opts)
	if err = c.dropIndexEntries(ctx, w, id, old, relaxed); err != nil {
		return err
	}
	if err = c.claimUniques(ctx, w, id, rec, relaxed); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode %s/%s %q: %w", c.ns, c.table, id, err)
	}
	return w.PutRaw(ctx, c.ns, c.table, id, raw, relaxed)
}

// Delete removes id; an absent id is idempotent success, never ErrNotFound.
func (c *Collection[R]) Delete(ctx context.Context, w Writer, id string, opts ...WriteOpt) error {
	raw, ok, err := w.GetRaw(ctx, c.ns, c.table, id)
	if err != nil || !ok {
		return err
	}
	old, err := c.decode(id, raw)
	if err != nil {
		return err
	}
	relaxed := relaxedOK(opts)
	if err := c.dropIndexEntries(ctx, w, id, old, relaxed); err != nil {
		return err
	}
	return w.DeleteRaw(ctx, c.ns, c.table, id, relaxed)
}

// Scan yields detached records in the engine's stable order; fn errors abort and propagate.
func (c *Collection[R]) Scan(ctx context.Context, r Reader, fn func(id string, rec *R) error) error {
	return r.ScanRaw(ctx, c.ns, c.table, func(id string, raw json.RawMessage) error {
		rec, err := c.decode(id, raw)
		if err != nil {
			return err
		}
		return fn(id, rec)
	})
}

// List returns all records detached; intended for small namespaces.
func (c *Collection[R]) List(ctx context.Context, r Reader) (map[string]*R, error) {
	out := map[string]*R{}
	if err := c.Scan(ctx, r, func(id string, rec *R) error {
		out[id] = rec
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Find resolves value through a declared unique index; a missing entry is ErrNotFound.
func (c *Collection[R]) Find(ctx context.Context, r Reader, index, value string) (string, *R, error) {
	if _, ok := c.uniques[index]; !ok {
		return "", nil, fmt.Errorf("%s/%s: unknown unique index %q", c.ns, c.table, index)
	}
	raw, ok, err := r.GetRaw(ctx, c.ns, c.indexTable(index), value)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("%s/%s %s=%q: %w", c.ns, c.table, index, value, ErrNotFound)
	}
	var id string
	if err = json.Unmarshal(raw, &id); err != nil {
		return "", nil, fmt.Errorf("decode index %s/%s %q: %w", c.ns, index, value, err)
	}
	rec, err := c.Get(ctx, r, id)
	if err != nil {
		return "", nil, err
	}
	return id, rec, nil
}

// FindAll yields every record whose declared non-unique index matches value.
func (c *Collection[R]) FindAll(ctx context.Context, r Reader, index, value string, fn func(id string, rec *R) error) error {
	key, ok := c.indexes[index]
	if !ok {
		return fmt.Errorf("%s/%s: unknown index %q", c.ns, c.table, index)
	}
	return c.Scan(ctx, r, func(id string, rec *R) error {
		if key(rec) != value {
			return nil
		}
		return fn(id, rec)
	})
}

func (c *Collection[R]) decode(id string, raw json.RawMessage) (*R, error) {
	rec := new(R)
	if err := json.Unmarshal(raw, rec); err != nil {
		return nil, fmt.Errorf("decode %s/%s %q: %w", c.ns, c.table, id, err)
	}
	return rec, nil
}

// claimUniques writes rec's unique-index entries, failing ErrConflict when a
// key already maps to a different id; empty keys are sparse and skipped.
func (c *Collection[R]) claimUniques(ctx context.Context, w Writer, id string, rec *R, relaxed bool) error {
	for name, key := range c.uniques {
		k := key(rec)
		if k == "" {
			continue
		}
		if strings.Contains(k, indexSep) {
			return fmt.Errorf("%s/%s index %s key contains separator", c.ns, c.table, name)
		}
		raw, ok, err := w.GetRaw(ctx, c.ns, c.indexTable(name), k)
		if err != nil {
			return err
		}
		if ok {
			var holder string
			if err = json.Unmarshal(raw, &holder); err != nil {
				return fmt.Errorf("decode index %s/%s %q: %w", c.ns, name, k, err)
			}
			if holder != id {
				return fmt.Errorf("%s/%s %s=%q held by %q: %w", c.ns, c.table, name, k, holder, ErrConflict)
			}
			continue
		}
		entry, err := json.Marshal(id)
		if err != nil {
			return err
		}
		if err := w.PutRaw(ctx, c.ns, c.indexTable(name), k, entry, relaxed); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collection[R]) dropIndexEntries(ctx context.Context, w Writer, id string, old *R, relaxed bool) error {
	for name, key := range c.uniques {
		k := key(old)
		if k == "" {
			continue
		}
		raw, ok, err := w.GetRaw(ctx, c.ns, c.indexTable(name), k)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var holder string
		if err = json.Unmarshal(raw, &holder); err != nil {
			return fmt.Errorf("decode index %s/%s %q: %w", c.ns, name, k, err)
		}
		if holder != id {
			continue
		}
		if err = w.DeleteRaw(ctx, c.ns, c.indexTable(name), k, relaxed); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collection[R]) indexTable(name string) string {
	return c.table + ".idx." + name
}
