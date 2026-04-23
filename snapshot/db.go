package snapshot

import (
	"errors"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

var (
	ErrNotFound = errors.New("snapshot not found")
)

// SnapshotRecord is the persisted record for a single snapshot.
type SnapshotRecord struct {
	types.Snapshot
	Pending bool   `json:"pending,omitempty"` // true while Create is in progress
	DataDir string `json:"data_dir,omitempty"`
}

// SnapshotIndex is the top-level DB structure for the snapshot module.
type SnapshotIndex struct {
	Snapshots map[string]*SnapshotRecord `json:"snapshots"`
	Names     map[string]string          `json:"names"` // name → snapshot ID
}

// Init implements storage.Initer.
func (idx *SnapshotIndex) Init() {
	utils.InitNamedIndex(&idx.Snapshots, &idx.Names)
}

// Resolve resolves a ref (exact ID, name, or ID prefix ≥3 chars) to a full snapshot ID.
func (idx *SnapshotIndex) Resolve(ref string) (string, error) {
	return utils.ResolveRef(idx.Snapshots, idx.Names, ref, ErrNotFound)
}

// ResolveMany batch-resolves refs to exact snapshot IDs, deduplicating results.
func (idx *SnapshotIndex) ResolveMany(refs []string) ([]string, error) {
	return utils.ResolveRefs(idx.Snapshots, idx.Names, refs, ErrNotFound)
}
