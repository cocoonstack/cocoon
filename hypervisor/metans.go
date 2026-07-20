package hypervisor

import (
	"encoding/json"
	"maps"
	"slices"
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

// rawVMIndex mirrors VMIndex's field layout with pass-through record bytes.
type rawVMIndex struct {
	VMs        map[string]json.RawMessage `json:"vms"`
	Names      map[string]json.RawMessage `json:"names"`
	OrphanDirs []string                   `json:"orphan_dirs,omitempty"`
	Tombstones map[string]json.RawMessage `json:"tombstones,omitempty"`
}

var _ metajson.Codec = vmIndexCodec{}

// vmIndexCodec reproduces the legacy VMIndex file byte-for-byte. Records
// cross as raw messages — one whole-file parse per load, no per-record
// re-marshal — matching the legacy store's JSON work per write; the map
// fields marshal sorted exactly as encoding/json always wrote them, and
// orphan_dirs keeps its slice order.
type vmIndexCodec struct{}

func (vmIndexCodec) Decode(data []byte) (*metajson.Model, error) {
	m := metajson.NewModel()
	if data == nil {
		return m, nil
	}
	var idx rawVMIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(idx.VMs)) {
		m.Put(tableRecords, id, idx.VMs[id])
	}
	for _, name := range slices.Sorted(maps.Keys(idx.Names)) {
		m.Put(tableNames, name, idx.Names[name])
	}
	for _, dir := range idx.OrphanDirs {
		m.Put(tableOrphanDirs, dir, json.RawMessage(orphanDirEntry))
	}
	for _, id := range slices.Sorted(maps.Keys(idx.Tombstones)) {
		m.Put(tableTombstones, id, idx.Tombstones[id])
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
