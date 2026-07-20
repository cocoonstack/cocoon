package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const resolvePrefixMin = 3

// NamedTx is the id→record set plus name→id index pattern shared by cocoon
// subsystems, viewed through one transaction handle. Get mirrors map lookup
// (nil when absent), Put is an upsert, and name entries are claimed and
// released explicitly — exactly the legacy index semantics.
type NamedTx[R any] struct {
	ctx   context.Context
	ns    string
	table string
	r     Reader
	w     Writer
	recs  *Collection[R]
	names *Collection[string]
}

// NewNamedTx binds the pattern to (ns, recordsTable, namesTable); w is nil in
// read-only transactions.
func NewNamedTx[R any](ctx context.Context, s Store, ns, recordsTable, namesTable string, r Reader, w Writer) *NamedTx[R] {
	return &NamedTx[R]{
		ctx:   ctx,
		ns:    ns,
		table: recordsTable,
		r:     r,
		w:     w,
		recs:  NewCollection[R](s, ns, recordsTable),
		names: NewCollection[string](s, ns, namesTable),
	}
}

// Get mirrors items[id]: nil when absent.
func (x *NamedTx[R]) Get(id string) (*R, error) {
	rec, err := x.recs.Get(x.ctx, x.r, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return rec, err
}

// Put mirrors items[id] = rec (upsert).
func (x *NamedTx[R]) Put(id string, rec *R, opts ...WriteOpt) error {
	err := x.recs.Replace(x.ctx, x.w, id, rec, opts...)
	if errors.Is(err, ErrNotFound) {
		return x.recs.Insert(x.ctx, x.w, id, rec, opts...)
	}
	return err
}

// Del mirrors delete(items, id).
func (x *NamedTx[R]) Del(id string) error {
	return x.recs.Delete(x.ctx, x.w, id)
}

// NameGet mirrors names[name] lookup.
func (x *NamedTx[R]) NameGet(name string) (string, bool, error) {
	id, err := x.names.Get(x.ctx, x.r, name)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return *id, true, nil
}

// NameSet mirrors names[name] = id.
func (x *NamedTx[R]) NameSet(name, id string, opts ...WriteOpt) error {
	err := x.names.Replace(x.ctx, x.w, name, &id, opts...)
	if errors.Is(err, ErrNotFound) {
		return x.names.Insert(x.ctx, x.w, name, &id, opts...)
	}
	return err
}

// NameDel mirrors delete(names, name).
func (x *NamedTx[R]) NameDel(name string) error {
	return x.names.Delete(x.ctx, x.w, name)
}

// All returns every record detached, keyed by id.
func (x *NamedTx[R]) All() (map[string]*R, error) {
	return x.recs.List(x.ctx, x.r)
}

// Scan yields detached records.
func (x *NamedTx[R]) Scan(fn func(id string, rec *R) error) error {
	return x.recs.Scan(x.ctx, x.r, fn)
}

// Resolve ports utils.ResolveRef: exact ID, then name, then ID prefix of at
// least three characters; notFound is the subsystem's sentinel.
func (x *NamedTx[R]) Resolve(ref string, notFound error) (string, error) {
	if rec, err := x.Get(ref); err != nil {
		return "", err
	} else if rec != nil {
		return ref, nil
	}
	if id, ok, err := x.NameGet(ref); err != nil {
		return "", err
	} else if ok {
		if rec, err := x.Get(id); err != nil {
			return "", err
		} else if rec != nil {
			return id, nil
		}
	}
	if len(ref) >= resolvePrefixMin {
		match := ""
		ambiguous := false
		if err := x.r.ScanRaw(x.ctx, x.ns, x.table, func(id string, _ json.RawMessage) error {
			if strings.HasPrefix(id, ref) {
				if match != "" {
					ambiguous = true
				}
				match = id
			}
			return nil
		}); err != nil {
			return "", err
		}
		if ambiguous {
			return "", fmt.Errorf("ambiguous ref %q: multiple matches", ref)
		}
		if match != "" {
			return match, nil
		}
	}
	return "", notFound
}

// ResolveMany ports utils.ResolveRefs: batch resolve with dedup.
func (x *NamedTx[R]) ResolveMany(refs []string, notFound error) ([]string, error) {
	seen := make(map[string]struct{}, len(refs))
	var ids []string
	for _, ref := range refs {
		id, err := x.Resolve(ref, notFound)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", ref, err)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// Reader exposes the transaction's read handle for satellite tables.
func (x *NamedTx[R]) Reader() Reader { return x.r }

// Writer exposes the transaction's write handle for satellite tables.
func (x *NamedTx[R]) Writer() Writer { return x.w }
