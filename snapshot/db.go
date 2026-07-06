package snapshot

import (
	"errors"
	"time"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// ErrNotFound is returned when a snapshot ref matches no record.
var ErrNotFound = errors.New("snapshot not found")

// SnapshotRecord is the persisted record for a single snapshot.
type SnapshotRecord struct {
	types.Snapshot
	DataDir        string    `json:"data_dir,omitempty"`
	SizeBytes      int64     `json:"size_bytes,omitempty"`
	Pending        bool      `json:"pending,omitempty"` // true while Create is in progress
	LastAccessedAt time.Time `json:"last_accessed_at,omitzero"`
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
