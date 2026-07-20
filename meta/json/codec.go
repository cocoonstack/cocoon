package json

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// Codec maps between a namespace file's bytes and its Model. Legacy
// namespaces provide codecs reproducing today's exact formats; Encode output
// is the complete file content including the trailing newline.
type Codec interface {
	// Decode parses file bytes; data == nil means the file does not exist.
	Decode(data []byte) (*Model, error)
	Encode(m *Model) ([]byte, error)
}

var _ Codec = GenericCodec{}

// GenericCodec persists a Model as {"tables":{...}} for
// namespaces with no legacy format (contract tests, future additions).
// Insertion order is not preserved across reload: tables refill sorted by id.
type GenericCodec struct{}

func (GenericCodec) Decode(data []byte) (*Model, error) {
	m := NewModel()
	if data == nil {
		return m, nil
	}
	var file genericFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("decode generic namespace: %w", err)
	}
	for _, tbl := range slices.Sorted(maps.Keys(file.Tables)) {
		recs := file.Tables[tbl]
		for _, id := range slices.Sorted(maps.Keys(recs)) {
			m.Put(tbl, id, recs[id])
		}
	}
	return m, nil
}

func (GenericCodec) Encode(m *Model) ([]byte, error) {
	file := genericFile{Tables: map[string]map[string]json.RawMessage{}}
	for _, tbl := range m.TableNames() {
		recs := map[string]json.RawMessage{}
		if err := m.Scan(tbl, func(id string, raw json.RawMessage) error {
			recs[id] = raw
			return nil
		}); err != nil {
			return nil, err
		}
		file.Tables[tbl] = recs
	}
	data, err := json.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("encode generic namespace: %w", err)
	}
	return append(data, '\n'), nil
}

type genericFile struct {
	Tables map[string]map[string]json.RawMessage `json:"tables"`
}
