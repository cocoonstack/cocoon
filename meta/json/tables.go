package json

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
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

// TableCodec is the declaration-only codec for pure table-shaped namespaces.
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
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.ReadToken(); err != nil {
		return nil, err
	} else if tok.Kind() != '{' {
		return nil, fmt.Errorf("namespace file: want object, got %v", tok)
	}
	for dec.PeekKind() != '}' {
		keyTok, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		if sp, ok := byKey[keyTok.String()]; ok {
			var tbl map[string]jsontext.Value
			if err := json.UnmarshalDecode(dec, &tbl); err != nil {
				return nil, err
			}
			for _, id := range slices.Sorted(maps.Keys(tbl)) {
				m.Put(sp.Table, id, tbl[id])
			}
			continue
		}
		if err := dec.SkipValue(); err != nil {
			return nil, err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	// Legacy json.Unmarshal rejected trailing bytes; a truncated-then-appended main must fall back to .prev, not decode (§9 format fidelity).
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
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
		if buf, err = appendTable(buf, m, sp.Table); err != nil {
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
