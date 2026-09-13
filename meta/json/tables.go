package json

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
)

// TableSpec maps one legacy top-level object field to a model table; Optional omits the field from encoded output while the table is empty.
type TableSpec struct {
	Key      string
	Table    string
	Optional bool
}

var _ Codec = TableCodec{}

// TableCodec is the declaration-only codec for pure table-shaped namespaces: a subsystem states its legacy field layout and owns no codec code.
type TableCodec struct {
	Specs []TableSpec
}

func (c TableCodec) Decode(data []byte) (*Model, error) {
	m := NewModel()
	if data == nil {
		return m, nil
	}
	byKey := make(map[string]TableSpec, len(c.Specs))
	for _, sp := range c.Specs {
		byKey[sp.Key] = sp
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if tok != json.Delim('{') {
		return nil, fmt.Errorf("namespace file: want object, got %v", tok)
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyTok.(string)
		if sp, ok := byKey[key]; ok {
			var tbl map[string]json.RawMessage
			if err := dec.Decode(&tbl); err != nil {
				return nil, err
			}
			for _, id := range slices.Sorted(maps.Keys(tbl)) {
				m.Put(sp.Table, id, tbl[id])
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	// Legacy json.Unmarshal rejected trailing bytes; a truncated-then-appended main must fall back to .prev, not decode (§9 format fidelity).
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("namespace file: trailing data after document")
	}
	return m, nil
}

func (c TableCodec) Encode(m *Model) ([]byte, error) {
	buf := []byte{'{'}
	for _, sp := range c.Specs {
		if sp.Optional && m.Len(sp.Table) == 0 {
			continue
		}
		buf = appendKey(buf, sp.Key)
		var err error
		if buf, err = AppendTable(buf, m, sp.Table); err != nil {
			return nil, err
		}
	}
	return append(buf, '}', '\n'), nil
}

func appendKey(buf []byte, key string) []byte {
	if len(buf) > 1 {
		buf = append(buf, ',')
	}
	buf = append(buf, '"')
	buf = append(buf, key...)
	return append(buf, '"', ':')
}
