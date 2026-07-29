package meta

import (
	"context"
	"encoding/json"
	"fmt"
)

// Collection is a typed record set inside one namespace table; reads hand back detached values, persisting a change requires Replace.
type Collection[R any] struct {
	ns    string
	table string
}

// NewCollection binds a Collection to a (namespace, table) pair.
func NewCollection[R any](ns, table string) *Collection[R] {
	return &Collection[R]{ns: ns, table: table}
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

// Insert adds a new record; an existing id is ErrConflict.
func (c *Collection[R]) Insert(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error {
	if _, ok, err := w.GetRaw(ctx, c.ns, c.table, id); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s/%s %q exists: %w", c.ns, c.table, id, ErrConflict)
	}
	return c.put(ctx, w, id, rec, opts)
}

// Replace overwrites an existing record; an absent id is ErrNotFound.
func (c *Collection[R]) Replace(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error {
	if _, ok, err := w.GetRaw(ctx, c.ns, c.table, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s/%s %q: %w", c.ns, c.table, id, ErrNotFound)
	}
	return c.put(ctx, w, id, rec, opts)
}

// Upsert inserts or replaces id (map-assignment semantics).
func (c *Collection[R]) Upsert(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error {
	return c.put(ctx, w, id, rec, opts)
}

// Delete removes id; an absent id is idempotent success, never ErrNotFound.
func (c *Collection[R]) Delete(ctx context.Context, w Writer, id string, opts ...WriteOpt) error {
	return w.DeleteRaw(ctx, c.ns, c.table, id, relaxedOK(opts))
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

func (c *Collection[R]) put(ctx context.Context, w Writer, id string, rec *R, opts []WriteOpt) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode %s/%s %q: %w", c.ns, c.table, id, err)
	}
	return w.PutRaw(ctx, c.ns, c.table, id, raw, relaxedOK(opts))
}

func (c *Collection[R]) decode(id string, raw json.RawMessage) (*R, error) {
	rec := new(R)
	if err := json.Unmarshal(raw, rec); err != nil {
		return nil, fmt.Errorf("decode %s/%s %q: %w", c.ns, c.table, id, err)
	}
	return rec, nil
}
