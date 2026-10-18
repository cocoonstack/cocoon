// Package disk is the runtime attach interface for extra virtio-blk disks backed by existing raw files or a snapshot's COW copy; attach is runtime-only (snapshot/hibernate refuse while one is attached) and detach deletes only cocoon's own copy.
package disk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/cocoon/types"
)

const (
	diskIDPrefix       = "cocoon-disk-"
	snapshotNamePrefix = "snap-"
	snapshotNameIDLen  = 8
)

// ErrUnsupportedBackend signals the backend cannot hot-plug virtio-blk disks (Firecracker on the MMIO transport).
var ErrUnsupportedBackend = errors.New("backend does not support disk attach")

// Spec is one attach request for an existing raw disk file; DirectIO nil means the create-path default (O_DIRECT unless writable disks have it disabled), tmpfs-backed files need an explicit off.
type Spec struct {
	Path     string
	Name     string
	ReadOnly bool
	DirectIO *bool
}

// Normalize enforces required fields; Name doubles as the guest serial (/dev/disk/by-id/virtio-<name>) and the detach key.
func (s *Spec) Normalize() error {
	if s.Path == "" {
		return fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(s.Path) {
		return fmt.Errorf("path must be absolute, got %q", s.Path)
	}
	return ValidateName(s.Name)
}

// SnapshotSource names a snapshot dir whose COW disk is copied and attached; BasePath is the local qcow2 base blob, empty for OCI.
type SnapshotSource struct {
	Dir      string
	Name     string
	BlobIDs  map[string]struct{}
	BasePath string
}

func (s *SnapshotSource) Normalize() error {
	if s.Dir == "" {
		return fmt.Errorf("snapshot dir is required")
	}
	return ValidateName(s.Name)
}

// Attached is the inspect-time view of one hot-added disk read from the running VMM's config.
type Attached struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readonly,omitzero"`
}

// Attacher hot-plugs and removes virtio-blk data disks on a running VM; both calls are idempotent.
type Attacher interface {
	DiskAttach(ctx context.Context, vmRef string, spec Spec) (deviceID string, err error)
	DiskDetach(ctx context.Context, vmRef, name string) error
}

// SnapshotAttacher hot-plugs a private writable copy of a snapshot's COW disk; repeating the call returns the attached disk.
type SnapshotAttacher interface {
	DiskAttachSnapshot(ctx context.Context, vmRef string, src SnapshotSource) (deviceID, format string, err error)
}

// Lister enumerates hot-added disks from running VM state.
type Lister interface {
	DiskList(ctx context.Context, vmRef string) ([]Attached, error)
}

// ValidateName checks a disk name, which doubles as the guest serial and a file name component.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if !types.ValidDataDiskName(name) {
		return fmt.Errorf("name %q invalid: must match ^[a-z][a-z0-9_-]{0,19}$", name)
	}
	return nil
}

// SnapshotName is the disk name of a snapshot-sourced attach; derived from the snapshot so two snapshots never collide on one name.
func SnapshotName(snapID string) string {
	return snapshotNamePrefix + strings.ToLower(snapID[:min(len(snapID), snapshotNameIDLen)])
}

// DeriveID returns the deterministic Cloud Hypervisor device id for a disk name (attach and detach share it so concurrent attaches collide on CH's id check); Firecracker derives its own underscore form.
func DeriveID(name string) string {
	return diskIDPrefix + name
}

// NameFromID reverses DeriveID; empty when id is not a hot-added disk (foreign same-prefix ids with an illegal name suffix are not ours).
func NameFromID(id string) string {
	name, ok := strings.CutPrefix(id, diskIDPrefix)
	if ok && types.ValidDataDiskName(name) {
		return name
	}
	return ""
}
