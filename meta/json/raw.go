package json

import (
	"encoding/json"
	"maps"
	"slices"
)

// AppendRawMap appends m as a compact JSON object with sorted keys, emitting
// values verbatim. Byte-identical to encoding/json marshaling the same map,
// minus its per-value compaction scan — the reason codecs use it (the legacy
// store paid one typed marshal per write; a validation re-scan of every
// record would be a new O(bytes) cost on the same path).
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

// CollectTable snapshots one table into a map of raw records.
func CollectTable(m *Model, tbl string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, m.Len(tbl))
	_ = m.Scan(tbl, func(id string, raw json.RawMessage) error {
		out[id] = raw
		return nil
	})
	return out
}
