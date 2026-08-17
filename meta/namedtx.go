package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const resolvePrefixMin = 3

// RecordTx is the id→record map view of one table inside a transaction: Get mirrors map lookup (nil when absent), Put is an upsert.
type RecordTx[R any] struct {
	ctx  context.Context
	r    Reader
	w    Writer
	recs *Collection[R]
}

// NewRecordTx binds the view to (ns, table); w is nil in read-only transactions.
func NewRecordTx[R any](ctx context.Context, ns, table string, r Reader, w Writer) *RecordTx[R] {
	return &RecordTx[R]{ctx: ctx, r: r, w: w, recs: NewCollection[R](ns, table)}
}

// Get mirrors items[id]: nil when absent.
func (x *RecordTx[R]) Get(id string) (*R, error) {
	rec, err := x.recs.Get(x.ctx, x.r, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return rec, err
}

// Put mirrors items[id] = rec (upsert).
func (x *RecordTx[R]) Put(id string, rec *R, opts ...WriteOpt) error {
	return x.recs.Upsert(x.ctx, x.w, id, rec, opts...)
}

// Del mirrors delete(items, id).
func (x *RecordTx[R]) Del(id string) error {
	return x.recs.Delete(x.ctx, x.w, id)
}

// All returns every record detached, keyed by id.
func (x *RecordTx[R]) All() (map[string]*R, error) {
	return x.recs.List(x.ctx, x.r)
}

// Scan yields detached records.
func (x *RecordTx[R]) Scan(fn func(id string, rec *R) error) error {
	return x.recs.Scan(x.ctx, x.r, fn)
}

// Reader exposes the transaction's read handle for satellite tables.
func (x *RecordTx[R]) Reader() Reader { return x.r }

// Writer exposes the transaction's write handle for satellite tables.
func (x *RecordTx[R]) Writer() Writer { return x.w }

// NamedTx is RecordTx plus an explicit name→id index shared by cocoon subsystems; name entries are claimed and released explicitly.
type NamedTx[R any] struct {
	*RecordTx[R]
	names *Collection[string]
}

// NewNamedTx binds the pattern to (ns, recordsTable, namesTable); w is nil in read-only transactions.
func NewNamedTx[R any](ctx context.Context, ns, recordsTable, namesTable string, r Reader, w Writer) *NamedTx[R] {
	return &NamedTx[R]{
		RecordTx: NewRecordTx[R](ctx, ns, recordsTable, r, w),
		names:    NewCollection[string](ns, namesTable),
	}
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
	return x.names.Upsert(x.ctx, x.w, name, &id, opts...)
}

// NameDel mirrors delete(names, name).
func (x *NamedTx[R]) NameDel(name string) error {
	return x.names.Delete(x.ctx, x.w, name)
}

// NameDelIfOwned removes name's mapping only while it still points at id; "" is a no-op.
func (x *NamedTx[R]) NameDelIfOwned(name, id string) error {
	if name == "" {
		return nil
	}
	cur, ok, err := x.NameGet(name)
	if err != nil || !ok || cur != id {
		return err
	}
	return x.NameDel(name)
}

// Resolve ports utils.ResolveRef: exact ID, then name, then ID prefix of at least three characters; notFound is the subsystem's sentinel.
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
		if err := x.r.ScanRaw(x.ctx, x.recs.ns, x.recs.table, func(id string, _ json.RawMessage) error {
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
