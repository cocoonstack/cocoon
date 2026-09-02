package meta

import (
	"context"
	"encoding/json"
	"fmt"
)

// seqCursorID is the reserved in-table row holding the last assigned Seq.
const seqCursorID = "cursor"

// Seq numbers committed log entries: unique and strictly increasing. Rolled-back numbers may be reused, and engines may leave gaps.
type Seq uint64

// Log is an append-only typed sequence over one namespace table; the cursor shares the table under a reserved id, so a rollback releases its number.
type Log[R any] struct {
	ns    string
	table string
}

// NewLog binds a Log to a (namespace, table) pair.
func NewLog[R any](ns, table string) *Log[R] {
	return &Log[R]{ns: ns, table: table}
}

// Append assigns the next Seq and writes rec under it.
func (l *Log[R]) Append(ctx context.Context, w Writer, rec *R, opts ...WriteOpt) (Seq, error) {
	raw, ok, err := w.GetRaw(ctx, l.ns, l.table, seqCursorID)
	if err != nil {
		return 0, err
	}
	var cur Seq
	if ok {
		if uerr := json.Unmarshal(raw, &cur); uerr != nil {
			return 0, fmt.Errorf("%s/%s cursor: %v: %w", l.ns, l.table, uerr, ErrCorrupt)
		}
	}
	next := cur + 1
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("encode %s/%s seq %d: %w", l.ns, l.table, next, err)
	}
	if perr := w.PutRaw(ctx, l.ns, l.table, seqID(next), data, relaxedOK(opts)); perr != nil {
		return 0, perr
	}
	curRaw, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	if perr := w.PutRaw(ctx, l.ns, l.table, seqCursorID, curRaw, relaxedOK(opts)); perr != nil {
		return 0, perr
	}
	return next, nil
}

func seqID(s Seq) string {
	return fmt.Sprintf("%020d", s)
}
