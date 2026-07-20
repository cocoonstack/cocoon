package json

import (
	"encoding/json"
	"maps"
	"slices"
)

// AppendRawMap appends m as a compact JSON object with sorted keys, values
// emitted verbatim — byte-identical to encoding/json without its per-value
// compaction re-scan.
func AppendRawMap(dst []byte, m map[string]json.RawMessage) ([]byte, error) {
	dst = append(dst, '{')
	for i, k := range slices.Sorted(maps.Keys(m)) {
		if i > 0 {
			dst = append(dst, ',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		dst = append(dst, kb...)
		dst = append(dst, ':')
		dst = append(dst, m[k]...)
	}
	return append(dst, '}'), nil
}

// AppendStringSlice appends s as a compact JSON array of strings.
func AppendStringSlice(dst []byte, s []string) ([]byte, error) {
	dst = append(dst, '[')
	for i, v := range s {
		if i > 0 {
			dst = append(dst, ',')
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		dst = append(dst, vb...)
	}
	return append(dst, ']'), nil
}

// AppendTable appends tbl as a compact JSON object with sorted keys, values
// verbatim — no intermediate value-map copy on the commit path.
func AppendTable(dst []byte, m *Model, tbl string) ([]byte, error) {
	t := m.tables[tbl]
	dst = append(dst, '{')
	if t != nil {
		for i, k := range slices.Sorted(maps.Keys(t.recs)) {
			if i > 0 {
				dst = append(dst, ',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			dst = append(dst, kb...)
			dst = append(dst, ':')
			dst = append(dst, t.recs[k]...)
		}
	}
	return append(dst, '}'), nil
}
