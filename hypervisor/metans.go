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

	orphanDirEntry = "{}"
)

// VMNamespaceName maps a backend type to its meta namespace (design §2).
func VMNamespaceName(typ string) string {
	return "vms_" + strings.ReplaceAll(typ, "-", "")
}

// MetaNamespace declares a backend's namespace over its legacy vms.json.
func MetaNamespace(typ string, conf BackendConfig) metajson.Namespace {
	return metajson.Namespace{
		Name:     VMNamespaceName(typ),
		FilePath: conf.IndexFile(),
		LockPath: conf.IndexLock(),
		Codec:    vmIndexCodec{},
	}
}

var _ metajson.Codec = vmIndexCodec{}

// vmIndexCodec reproduces the legacy VMIndex file byte-for-byte. Records
// cross as raw messages — one whole-file parse per load, no per-record
// re-marshal — matching the legacy store's JSON work per write; the map
// fields marshal sorted exactly as encoding/json always wrote them, and
// orphan_dirs keeps its slice order.
type vmIndexCodec struct{}

// rawVMIndex mirrors VMIndex's field layout with pass-through record bytes.
type rawVMIndex struct {
	VMs        map[string]json.RawMessage `json:"vms"`
	Names      map[string]json.RawMessage `json:"names"`
	OrphanDirs []string                   `json:"orphan_dirs,omitempty"`
}

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
	return m, nil
}

func (vmIndexCodec) Encode(m *metajson.Model) ([]byte, error) {
	idx := rawVMIndex{VMs: map[string]json.RawMessage{}, Names: map[string]json.RawMessage{}}
	if err := m.Scan(tableRecords, func(id string, raw json.RawMessage) error {
		idx.VMs[id] = raw
		return nil
	}); err != nil {
		return nil, err
	}
	if err := m.Scan(tableNames, func(name string, raw json.RawMessage) error {
		idx.Names[name] = raw
		return nil
	}); err != nil {
		return nil, err
	}
	if err := m.Scan(tableOrphanDirs, func(dir string, _ json.RawMessage) error {
		idx.OrphanDirs = append(idx.OrphanDirs, dir)
		return nil
	}); err != nil {
		return nil, err
	}
	data, err := json.Marshal(&idx)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
