package hypervisor

import (
	"encoding/json"
	"strings"

	metajson "github.com/cocoonstack/cocoon/meta/json"
)

const (
	tableRecords    = "records"
	tableNames      = "names"
	tableOrphanDirs = "orphandirs"
	tableTombstones = "tombstones"

	orphanDirEntry = "{}"
)

// VMNamespaceName maps a backend type to its meta namespace (design §2).
func VMNamespaceName(typ string) string {
	return "vms_" + strings.ReplaceAll(typ, "-", "")
}

// MetaNamespace declares a backend's namespace over its legacy vms.json.
func MetaNamespace(typ, indexFile, lockPath string) metajson.Namespace {
	return metajson.Namespace{
		Name:     VMNamespaceName(typ),
		FilePath: indexFile,
		LockPath: lockPath,
		Codec:    vmIndexCodec{},
	}
}

var vmTables = []metajson.TableSpec{
	{Key: "vms", Table: tableRecords},
	{Key: "names", Table: tableNames},
	{Key: "tombstones", Table: tableTombstones, Optional: true},
}

var _ metajson.Codec = vmIndexCodec{}

// vmIndexCodec reproduces the legacy VMIndex file byte-for-byte. Records
// cross as raw messages — one whole-file parse per load, no per-record
// re-marshal — matching the legacy store's JSON work per write; the map
// fields marshal sorted exactly as encoding/json always wrote them, and
// orphan_dirs keeps its slice order.
type vmIndexCodec struct{}

func (vmIndexCodec) Decode(data []byte) (*metajson.Model, error) {
	m, top, err := metajson.DecodeTables(data, vmTables)
	if err != nil {
		return nil, err
	}
	// orphan_dirs is a slice, not a table: preserve its order outside specs.
	if raw, ok := top["orphan_dirs"]; ok {
		var dirs []string
		if err := json.Unmarshal(raw, &dirs); err != nil {
			return nil, err
		}
		for _, dir := range dirs {
			m.Put(tableOrphanDirs, dir, json.RawMessage(orphanDirEntry))
		}
	}
	return m, nil
}

func (vmIndexCodec) Encode(m *metajson.Model) ([]byte, error) {
	buf := append([]byte(nil), `{"vms":`...)
	buf, err := metajson.AppendRawMap(buf, metajson.CollectTable(m, tableRecords))
	if err != nil {
		return nil, err
	}
	buf = append(buf, `,"names":`...)
	if buf, err = metajson.AppendRawMap(buf, metajson.CollectTable(m, tableNames)); err != nil {
		return nil, err
	}
	var dirs []string
	if err = m.Scan(tableOrphanDirs, func(dir string, _ json.RawMessage) error {
		dirs = append(dirs, dir)
		return nil
	}); err != nil {
		return nil, err
	}
	if len(dirs) > 0 {
		buf = append(buf, `,"orphan_dirs":`...)
		if buf, err = metajson.AppendStringSlice(buf, dirs); err != nil {
			return nil, err
		}
	}
	if ts := metajson.CollectTable(m, tableTombstones); len(ts) > 0 {
		buf = append(buf, `,"tombstones":`...)
		if buf, err = metajson.AppendRawMap(buf, ts); err != nil {
			return nil, err
		}
	}
	return append(buf, '}', '\n'), nil
}
