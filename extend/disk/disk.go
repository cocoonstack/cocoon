// Package disk is the runtime attach interface for extra virtio-blk data
// disks backed by existing raw files. Attach is runtime-only: the disk is
// not part of the VM record, snapshot/hibernate of the VM is refused while
// one is attached (detach first), and detach never deletes the backing file.
package disk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cocoonstack/cocoon/types"
)

// ErrUnsupportedBackend signals the backend cannot hot-plug virtio-blk disks (e.g. Firecracker).
var ErrUnsupportedBackend = errors.New("backend does not support disk attach")

// Spec is one attach request: an existing raw disk file.
type Spec struct {
	Path     string
	Name     string
	ReadOnly bool
}

// Normalize enforces required fields; the name doubles as the guest serial
// (/dev/disk/by-id/virtio-<name>) and the detach key.
func (s *Spec) Normalize() error {
	if s.Path == "" {
		return fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(s.Path) {
		return fmt.Errorf("path must be absolute, got %q", s.Path)
	}
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !types.ValidDataDiskName(s.Name) {
		return fmt.Errorf("name %q invalid: must match ^[a-z][a-z0-9_-]{0,19}$", s.Name)
	}
	return nil
}

// Attached is the inspect-time view of one hot-added disk read from the running VM's CH config.
type Attached struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// Attacher hot-plugs and removes virtio-blk data disks on a running VM.
type Attacher interface {
	DiskAttach(ctx context.Context, vmRef string, spec Spec) (deviceID string, err error)
	DiskDetach(ctx context.Context, vmRef, name string) error
}

// Lister enumerates hot-added disks from running VM state.
type Lister interface {
	DiskList(ctx context.Context, vmRef string) ([]Attached, error)
}

// DeriveID returns the deterministic CH device id for a disk name (used by
// attach + detach so concurrent attaches collide on CH's id check).
func DeriveID(name string) string {
	return "cocoon-disk-" + name
}

// NameFromID reverses DeriveID; empty when id is not a hot-added disk
// (foreign same-prefix ids with an illegal name suffix are not ours).
func NameFromID(id string) string {
	const prefix = "cocoon-disk-"
	if len(id) > len(prefix) && id[:len(prefix)] == prefix && types.ValidDataDiskName(id[len(prefix):]) {
		return id[len(prefix):]
	}
	return ""
}
