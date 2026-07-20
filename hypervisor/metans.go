package hypervisor

import (
	"encoding/json"
	"fmt"
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

// vmIndexCodec reproduces the legacy VMIndex file byte-for-byte: map-backed
// tables re-marshal sorted (as encoding/json always wrote them) while
// orphan_dirs keeps its slice order.
type vmIndexCodec struct{}

func (vmIndexCodec) Decode(data []byte) (*metajson.Model, error) {
	m := metajson.NewModel()
	if data == nil {
		return m, nil
	}
	var idx VMIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(idx.VMs)) {
		raw, err := json.Marshal(idx.VMs[id])
		if err != nil {
			return nil, err
		}
		m.Put(tableRecords, id, raw)
	}
	for _, name := range slices.Sorted(maps.Keys(idx.Names)) {
		raw, err := json.Marshal(idx.Names[name])
		if err != nil {
			return nil, err
		}
		m.Put(tableNames, name, raw)
	}
	for _, dir := range idx.OrphanDirs {
		m.Put(tableOrphanDirs, dir, json.RawMessage(orphanDirEntry))
	}
	return m, nil
}

func (vmIndexCodec) Encode(m *metajson.Model) ([]byte, error) {
	idx := VMIndex{}
	idx.Init()
	if err := m.Scan(tableRecords, func(id string, raw json.RawMessage) error {
		var rec VMRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("decode vm record %s: %w", id, err)
		}
		idx.VMs[id] = &rec
		return nil
	}); err != nil {
		return nil, err
	}
	if err := m.Scan(tableNames, func(name string, raw json.RawMessage) error {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return fmt.Errorf("decode name entry %s: %w", name, err)
		}
		idx.Names[name] = id
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
