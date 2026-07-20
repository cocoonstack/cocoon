// Package json is the meta engine over today's per-namespace JSON files:
// same formats, same .prev crash story, same flocks (design §8).
package json

import (
	"encoding/json"
	"maps"
	"slices"
)

// Model is one namespace's decoded state: named tables plus log sequence
// counters. Tables preserve insertion order — loaded file order first, new
// ids appended — which legacy codecs rely on for order-sensitive fields.
type Model struct {
	tables map[string]*table
	seqs   map[string]uint64
}

// NewModel returns an empty namespace model.
func NewModel() *Model {
	return &Model{tables: map[string]*table{}, seqs: map[string]uint64{}}
}

// Get returns the raw record under (tbl, id).
func (m *Model) Get(tbl, id string) (json.RawMessage, bool) {
	t := m.tables[tbl]
	if t == nil {
		return nil, false
	}
	raw, ok := t.recs[id]
	return raw, ok
}

// Put inserts or overwrites (tbl, id); new ids append to the table's order.
func (m *Model) Put(tbl, id string, raw json.RawMessage) {
	t := m.tables[tbl]
	if t == nil {
		t = &table{recs: map[string]json.RawMessage{}}
		m.tables[tbl] = t
	}
	if _, ok := t.recs[id]; !ok {
		t.ids = append(t.ids, id)
	}
	t.recs[id] = raw
}

// Delete removes (tbl, id), preserving the relative order of the rest.
func (m *Model) Delete(tbl, id string) {
	t := m.tables[tbl]
	if t == nil {
		return
	}
	if _, ok := t.recs[id]; !ok {
		return
	}
	delete(t.recs, id)
	t.ids = slices.DeleteFunc(t.ids, func(s string) bool { return s == id })
}

// Scan yields (tbl, id) pairs in insertion order; fn errors abort and propagate.
func (m *Model) Scan(tbl string, fn func(id string, raw json.RawMessage) error) error {
	t := m.tables[tbl]
	if t == nil {
		return nil
	}
	for _, id := range t.ids {
		if err := fn(id, t.recs[id]); err != nil {
			return err
		}
	}
	return nil
}

// Len returns tbl's record count.
func (m *Model) Len(tbl string) int {
	t := m.tables[tbl]
	if t == nil {
		return 0
	}
	return len(t.ids)
}

// NextSeq advances and returns tbl's log sequence counter.
func (m *Model) NextSeq(tbl string) uint64 {
	m.seqs[tbl]++
	return m.seqs[tbl]
}

// Seq returns tbl's current log sequence counter.
func (m *Model) Seq(tbl string) uint64 { return m.seqs[tbl] }

// SetSeq restores tbl's log sequence counter (codec decode).
func (m *Model) SetSeq(tbl string, v uint64) { m.seqs[tbl] = v }

// TableNames returns all table names sorted; for generic codecs.
func (m *Model) TableNames() []string {
	return slices.Sorted(maps.Keys(m.tables))
}

// SeqTables returns all sequence-counter table names sorted; for generic codecs.
func (m *Model) SeqTables() []string {
	return slices.Sorted(maps.Keys(m.seqs))
}

type table struct {
	ids  []string
	recs map[string]json.RawMessage
}
