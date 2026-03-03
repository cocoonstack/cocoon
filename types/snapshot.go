package types

import "time"

// SnapshotConfig carries the parameters for creating a snapshot.
// The hypervisor fills Image, ImageBlobIDs, and resource fields; the CLI adds Name and Description.
type SnapshotConfig struct {
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	Image        string              `json:"image,omitempty"`          // source image reference (e.g. "ubuntu:22.04")
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"` // blob hex set for GC pinning

	// Original VM resource config, populated during snapshot creation.
	// Used by clone for parameter inheritance and validation.
	CPU     int   `json:"cpu,omitempty"`
	Memory  int64 `json:"memory,omitempty"`  // bytes
	Storage int64 `json:"storage,omitempty"` // bytes
	NICs    int   `json:"nics,omitempty"`
}

// Snapshot is the public record for a snapshot.
// Not bound to any specific VM — a snapshot can be restored to any VM.
type Snapshot struct {
	SnapshotConfig
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}
