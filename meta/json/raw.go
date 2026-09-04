package json

import (
	"encoding/json"
	"maps"
	"slices"
)

// AppendTable appends tbl as a compact JSON object with sorted keys, values verbatim.
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
