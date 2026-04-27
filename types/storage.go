package types

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	StorageRoleLayer  StorageRole = "layer"
	StorageRoleCOW    StorageRole = "cow"
	StorageRoleCidata StorageRole = "cidata"
	StorageRoleData   StorageRole = "data"

	// Phase 1 fstype values for Role==Data disks.
	FSTypeExt4 = "ext4"
	FSTypeNone = "none"
)

// dataDiskNameRe caps length at 20 to match Linux's
// /dev/disk/by-id/virtio-<first 20 chars> truncation.
var dataDiskNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,19}$`)

// StorageRole classifies a disk's purpose in the VM. Required on every
// StorageConfig — empty values are rejected by ValidateStorageConfigs.
type StorageRole string

// StorageConfig describes a disk attached to a VM.
type StorageConfig struct {
	Path       string      `json:"path"`
	RO         bool        `json:"ro"`
	Serial     string      `json:"serial"`
	Role       StorageRole `json:"role"`
	MountPoint string      `json:"mount_point,omitempty"` // Role==Data only
	FSType     string      `json:"fstype,omitempty"`      // Role==Data only
	DirectIO   *bool       `json:"direct_io,omitempty"`   // Role==Data only; nil inherits VM-level NoDirectIO
}

// DataDiskSpec is the user-facing description of an extra data disk parsed
// from --data-disk. Transient — never persisted.
type DataDiskSpec struct {
	Name          string
	Size          int64
	FSType        string
	MountPoint    string
	MountPointSet bool `json:"-"` // distinguishes mount=<empty> (set) from omitted
	DirectIO      *bool
}

// ValidateStorageConfigs enforces invariants at every boundary that loads
// or finalizes StorageConfigs (FinalizeCreate, Start, Snapshot, Clone/Restore
// after sidecar load).
func ValidateStorageConfigs(configs []*StorageConfig) error {
	for i, sc := range configs {
		if sc == nil {
			return fmt.Errorf("storage config %d: nil", i)
		}
		switch sc.Role {
		case StorageRoleLayer, StorageRoleCidata:
			if !sc.RO {
				return fmt.Errorf("storage config %d (%s): role %s requires RO=true", i, sc.Path, sc.Role)
			}
		case StorageRoleCOW, StorageRoleData:
			if sc.RO {
				return fmt.Errorf("storage config %d (%s): role %s requires RO=false", i, sc.Path, sc.Role)
			}
		default:
			return fmt.Errorf("storage config %d (%s): role must be one of layer/cow/cidata/data, got %q", i, sc.Path, sc.Role)
		}
		if sc.Role != StorageRoleData {
			if sc.DirectIO != nil {
				return fmt.Errorf("storage config %d (%s): direct_io only allowed on data disks", i, sc.Path)
			}
			continue
		}
		if !validDataDiskFSType(sc.FSType) {
			return fmt.Errorf("data disk %s: fstype must be ext4 or none, got %q", sc.Serial, sc.FSType)
		}
		if sc.FSType == FSTypeNone && sc.MountPoint != "" {
			return fmt.Errorf("data disk %s: fstype=none requires mount_point empty", sc.Serial)
		}
		if sc.MountPoint != "" {
			if !filepath.IsAbs(sc.MountPoint) {
				return fmt.Errorf("data disk %s: mount_point must be absolute, got %q", sc.Serial, sc.MountPoint)
			}
			if strings.ContainsAny(sc.MountPoint, "\x00\n") {
				return fmt.Errorf("data disk %s: mount_point contains forbidden characters", sc.Serial)
			}
		}
		if !ValidDataDiskName(sc.Serial) {
			return fmt.Errorf("data disk: serial %q does not match name rules", sc.Serial)
		}
	}
	return nil
}

// ValidDataDiskName reports whether s is a legal data disk name.
// Shared between CLI parsing and sidecar loading (sidecar may be untrusted).
func ValidDataDiskName(s string) bool {
	if !dataDiskNameRe.MatchString(s) {
		return false
	}
	if strings.HasPrefix(s, "cocoon-") {
		return false
	}
	return true
}

func validDataDiskFSType(t string) bool {
	return t == FSTypeExt4 || t == FSTypeNone
}
