package meta

import (
	"context"
	"encoding/json"
	"fmt"
)

// Log is an append-only entry sequence inside one namespace table. Over
// committed entries Seq is unique and strictly increasing; a number handed to
// a rolled-back transaction may be reused or burned by the engine.
type Log[E any] struct {
	ns    string
	table string
}

// NewLog binds a Log to a (namespace, table) pair.
func NewLog[E any](_ Store, ns, table string) *Log[E] {
	return &Log[E]{ns: ns, table: table}
}

// Append writes e under the next Seq; bound by the durability contract like collection writes.
func (l *Log[E]) Append(ctx context.Context, w Writer, e E, opts ...WriteOpt) (Seq, error) {
	seq, err := w.NextSeq(ctx, l.ns, l.table)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("encode %s/%s entry: %w", l.ns, l.table, err)
	}
	if err := w.PutRaw(ctx, l.ns, l.table, seqID(seq), raw, relaxedOK(opts)); err != nil {
		return 0, err
	}
	return seq, nil
}

// Scan yields committed entries with Seq strictly greater than after, in increasing order.
func (l *Log[E]) Scan(ctx context.Context, r Reader, after Seq, fn func(Seq, E) error) error {
	return r.ScanRaw(ctx, l.ns, l.table, func(id string, raw json.RawMessage) error {
		var seq Seq
		if _, err := fmt.Sscanf(id, "%020d", &seq); err != nil {
			return fmt.Errorf("decode %s/%s seq %q: %w", l.ns, l.table, id, err)
		}
		if seq <= after {
			return nil
		}
		var e E
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("decode %s/%s entry %d: %w", l.ns, l.table, seq, err)
		}
		return fn(seq, e)
	})
}

func seqID(seq Seq) string {
	return fmt.Sprintf("%020d", seq)
}
