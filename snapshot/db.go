package snapshot

import (
	"errors"
	"time"

	"github.com/cocoonstack/cocoon/types"
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
