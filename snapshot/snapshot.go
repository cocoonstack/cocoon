package snapshot

import (
	"context"
	"io"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/types"
)

// Direct is an optional interface for snapshot backends that expose the local data directory for per-file handling (hardlink, reflink, etc.).
type Direct interface {
	DataDir(ctx context.Context, ref string) (string, types.SnapshotConfig, error)
}

// CompressedExporter is an optional interface for backends that support exporting with compression (e.g. gzip). The default Export produces raw tar.
type CompressedExporter interface {
	ExportCompressed(ctx context.Context, ref string) (io.ReadCloser, error)
}

// DirectoryExporter exports into a target dir with snapshot.json so rsync/NFS workflows skip the tar round-trip. Pairs with `vm clone --from-dir`.
type DirectoryExporter interface {
	ExportToDir(ctx context.Context, ref, dir string) error
}

// Snapshot manages snapshot lifecycle and storage.
type Snapshot interface {
	Type() string

	// Create persists a snapshot from the given config and data stream, returning the snapshot ID.
	Create(ctx context.Context, cfg *types.SnapshotConfig, stream io.Reader) (string, error)
	// List returns all snapshots.
	List(ctx context.Context) ([]*types.Snapshot, error)
	// Inspect returns a single snapshot by ID or name.
	Inspect(ctx context.Context, ref string) (*types.Snapshot, error)
	// Delete removes snapshots by ID or name. Returns the list of actually deleted IDs.
	Delete(ctx context.Context, refs []string) ([]string, error)
	// Restore restores a snapshot by ID or name, returning the snapshot config and a data stream.
	Restore(ctx context.Context, ref string) (types.SnapshotConfig, io.ReadCloser, error)

	// Export streams the snapshot as a raw tar (snapshot.json entry first, then data files).
	Export(ctx context.Context, ref string) (io.ReadCloser, error)
	// Import reads a snapshot tar (gzip auto-detected); non-empty name/description override the envelope.
	Import(ctx context.Context, r io.Reader, name, description string) (string, error)

	RegisterGC(*gc.Orchestrator)
}
