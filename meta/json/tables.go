package json

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// stringListMarker backs StringList presence rows; the value never surfaces.
var stringListMarker = json.RawMessage("{}")

// TableSpec maps one legacy top-level object field to a model table;
// Optional omits the field from encoded output while the table is empty;
// StringList marks an ordered []string field stored as presence markers.
type TableSpec struct {
	Key        string
	Table      string
	Optional   bool
	StringList bool
}

// DecodeTables loads specs' map fields into a fresh Model (sorted insertion,
// matching what encoding/json always wrote) and returns non-spec top-level
// fields for codec-specific extras. Single streaming pass: a whole-file
// unmarshal into raw messages would tokenize the payload twice (the paired
// perf gate caught the O(N) regression).
func DecodeTables(data []byte, specs []TableSpec) (*Model, map[string]json.RawMessage, error) {
	m := NewModel()
	if data == nil {
		return m, nil, nil
	}
	byKey := make(map[string]TableSpec, len(specs))
	for _, sp := range specs {
		byKey[sp.Key] = sp
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil {
		return nil, nil, err
	} else if tok != json.Delim('{') {
		return nil, nil, fmt.Errorf("namespace file: want object, got %v", tok)
	}
	var extras map[string]json.RawMessage
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := keyTok.(string)
		if sp, ok := byKey[key]; ok {
			if sp.StringList {
				var ids []string
				if err := dec.Decode(&ids); err != nil {
					return nil, nil, err
				}
				for _, id := range ids {
					m.Put(sp.Table, id, stringListMarker)
				}
				continue
			}
			var tbl map[string]json.RawMessage
			if err := dec.Decode(&tbl); err != nil {
				return nil, nil, err
			}
			for _, id := range slices.Sorted(maps.Keys(tbl)) {
				m.Put(sp.Table, id, tbl[id])
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		if extras == nil {
			extras = map[string]json.RawMessage{}
		}
		extras[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	return m, extras, nil
}

// EncodeTables assembles the legacy object shape from specs' tables.
func EncodeTables(m *Model, specs []TableSpec) ([]byte, error) {
	buf := []byte{'{'}
	for _, sp := range specs {
		if sp.StringList {
			var ids []string
			_ = m.Scan(sp.Table, func(id string, _ json.RawMessage) error {
				ids = append(ids, id)
				return nil
			})
			if sp.Optional && len(ids) == 0 {
				continue
			}
			buf = appendKey(buf, sp.Key)
			var err error
			if buf, err = AppendStringSlice(buf, ids); err != nil {
				return nil, err
			}
			continue
		}
		collected := CollectTable(m, sp.Table)
		if sp.Optional && len(collected) == 0 {
			continue
		}
		buf = appendKey(buf, sp.Key)
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

func appendKey(buf []byte, key string) []byte {
	if len(buf) > 1 {
		buf = append(buf, ',')
	}
	buf = append(buf, '"')
	buf = append(buf, key...)
	return append(buf, '"', ':')
}
