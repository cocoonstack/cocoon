package hypervisor

import (
	"context"
	"errors"
	"io"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/types"
)

var (
	ErrNotFound   = errors.New("vm not found")
	ErrNotRunning = errors.New("vm not running")
	ErrAmbiguous  = errors.New("vm ref resolves to multiple backends")
)

// Hypervisor manages VM lifecycle. Implemented by each backend.
type Hypervisor interface {
	Type() string

	Create(ctx context.Context, vmID string, vmCfg *types.VMConfig, storage []*types.StorageConfig, net types.NetSetup, boot *types.BootConfig) (*types.VM, error)
	Start(ctx context.Context, refs []string) ([]string, error)
	Stop(ctx context.Context, refs []string) ([]string, error)
	Inspect(ctx context.Context, ref string) (*types.VM, error)
	List(context.Context) ([]*types.VM, error)
	Delete(ctx context.Context, refs []string, force bool) ([]string, error)
	Console(ctx context.Context, ref string) (io.ReadWriteCloser, error)
	LogPath(ctx context.Context, ref string) (string, error)
	Snapshot(ctx context.Context, ref string) (*types.SnapshotConfig, io.ReadCloser, error)
	Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, snapshot io.Reader) (*types.VM, error)
	Restore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, snapshot io.Reader, sourceSnapshotID string) (*types.VM, error)

	RegisterGC(*gc.Orchestrator)
}

// Watchable is optionally implemented by hypervisors that support file-based state watching.
type Watchable interface {
	WatchPath() string
}

// Direct is an optional interface for hypervisors that support clone/restore from a local snapshot directory.
type Direct interface {
	DirectClone(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error)
	DirectRestore(ctx context.Context, vmRef string, vmCfg *types.VMConfig, srcDir, sourceSnapshotID string) (*types.VM, error)
}

// Hibernator snapshots and stops atomically: capture, persist, and termination share one pause window, and the VMM dies only after persist succeeds — a failed persist leaves the VM running. persist consumes srcDir (the finalized capture dir) — moving it into a local store or streaming it out — and must return only once the snapshot is durable.
type Hibernator interface {
	Hibernate(ctx context.Context, ref string, persist func(cfg *types.SnapshotConfig, srcDir string) error) error
}
