package json

import (
	"encoding/json"
	"maps"
	"slices"
)

// TableSpec maps one legacy top-level object field to a model table;
// Optional omits the field from encoded output while the table is empty.
type TableSpec struct {
	Key      string
	Table    string
	Optional bool
}

// DecodeTables loads specs' map fields into a fresh Model (sorted insertion,
// matching what encoding/json always wrote) and returns the raw top level
// for codec-specific extras.
func DecodeTables(data []byte, specs []TableSpec) (*Model, map[string]json.RawMessage, error) {
	m := NewModel()
	if data == nil {
		return m, nil, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, nil, err
	}
	for _, sp := range specs {
		raw, ok := top[sp.Key]
		if !ok {
			continue
		}
		var tbl map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tbl); err != nil {
			return nil, nil, err
		}
		for _, id := range slices.Sorted(maps.Keys(tbl)) {
			m.Put(sp.Table, id, tbl[id])
		}
	}
	return m, top, nil
}

// EncodeTables assembles the legacy object shape from specs' tables.
func EncodeTables(m *Model, specs []TableSpec) ([]byte, error) {
	buf := []byte{'{'}
	for _, sp := range specs {
		collected := CollectTable(m, sp.Table)
		if sp.Optional && len(collected) == 0 {
			continue
		}
		if len(buf) > 1 {
			buf = append(buf, ',')
		}
		buf = append(buf, '"')
		buf = append(buf, sp.Key...)
		buf = append(buf, '"', ':')
		var err error
		if buf, err = AppendRawMap(buf, collected); err != nil {
			return nil, err
		}
	}
	return append(buf, '}', '\n'), nil
}

var _ Codec = TableCodec{}

// TableCodec is the declaration-only codec for pure table-shaped namespaces:
// a subsystem states its legacy field layout and owns no codec code.
type TableCodec struct {
	Specs []TableSpec
}

func (c TableCodec) Decode(data []byte) (*Model, error) {
	m, _, err := DecodeTables(data, c.Specs)
	return m, err
}

func (c TableCodec) Encode(m *Model) ([]byte, error) {
	return EncodeTables(m, c.Specs)
}
