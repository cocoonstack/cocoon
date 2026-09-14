package snapshot

import (
	"context"
	"io"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/types"
)

// Direct exposes the local data directory; the returned release ends the read lease that keeps delete/GC from reaping it and must be called exactly once.
type Direct interface {
	DataDir(ctx context.Context, ref string) (string, types.SnapshotConfig, func(), error)
}

// DirectCreator ingests a capture directory in place when srcDir shares a filesystem with the store; ok is false, srcDir intact, when it cannot.
type DirectCreator interface {
	CreateFromDir(ctx context.Context, cfg *types.SnapshotConfig, srcDir string) (id string, ok bool, err error)
}

// NameHolder reports a name held by a pending record Inspect does not resolve, so a save preflight rejects a taken name before the capture.
type NameHolder interface {
	NameOwner(ctx context.Context, name string) (id string, held bool, err error)
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

	Create(ctx context.Context, cfg *types.SnapshotConfig, stream io.Reader) (string, error)
	List(ctx context.Context) ([]*types.Snapshot, error)
	Inspect(ctx context.Context, ref string) (*types.Snapshot, error)
	// Delete removes snapshots by ID or name. Returns the list of actually deleted IDs.
	Delete(ctx context.Context, refs []string) ([]string, error)
	Restore(ctx context.Context, ref string) (types.SnapshotConfig, io.ReadCloser, error)

	// Export streams the snapshot as a raw tar (snapshot.json entry first, then data files).
	Export(ctx context.Context, ref string) (io.ReadCloser, error)
	// Import reads a snapshot tar (gzip auto-detected); non-empty name/description override the envelope.
	Import(ctx context.Context, r io.Reader, name, description string) (string, error)

	RegisterGC(*gc.Orchestrator)
}
