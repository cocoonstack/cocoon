package types

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/docker/go-units"
)

const (
	StorageRoleLayer  StorageRole = "layer"
	StorageRoleCOW    StorageRole = "cow"
	StorageRoleCidata StorageRole = "cidata"
	StorageRoleData   StorageRole = "data"

	FSTypeExt4 = "ext4"
	FSTypeNone = "none"

	// MinDataDiskSize floors a user data disk; mkfs.ext4 is unstable below it on small sparse files.
	MinDataDiskSize int64 = 16 << 20

	// COWRawFileName is the raw COW disk's file name in the run dir and the snapshot (single owner so snapshot matchers and clone path rewrites can't drift).
	COWRawFileName = "cow.raw"
)

// dataDiskNameRe caps length at 20 to match Linux's /dev/disk/by-id/virtio-<first 20 chars> truncation.
var dataDiskNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,19}$`)

// StorageRole classifies a disk's purpose; empty is rejected by ValidateStorageConfigs.
type StorageRole string

// StorageConfig describes a disk attached to a VM.
type StorageConfig struct {
	Path   string      `json:"path"`
	RO     bool        `json:"ro"`
	Serial string      `json:"serial"`
	Role   StorageRole `json:"role"`
	// MountPoint, FSType and DirectIO apply to Role==Data only; a nil DirectIO inherits the VM-level NoDirectIO.
	MountPoint string `json:"mount_point,omitempty"`
	FSType     string `json:"fstype,omitempty"`
	DirectIO   *bool  `json:"direct_io,omitempty"`
}

// DataDiskSpec is the user-facing description of an extra data disk parsed from --data-disk. Transient — never persisted.
type DataDiskSpec struct {
	Name          string
	Size          int64
	FSType        string
	MountPoint    string
	MountPointSet bool `json:"-"` // distinguishes mount=<empty> (set) from omitted
	DirectIO      *bool
}

// ValidateStorageConfigs enforces StorageConfig invariants at every load/finalize boundary.
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

// ValidDataDiskName reports whether s is a legal data disk name; shared with untrusted sidecar loading.
func ValidDataDiskName(s string) bool {
	return dataDiskNameRe.MatchString(s) && !strings.HasPrefix(s, "cocoon-")
}

// ParseDataDiskSpec parses one comma-separated key=value --data-disk value; size is required and floors at MinDataDiskSize.
func ParseDataDiskSpec(s string) (DataDiskSpec, error) {
	var spec DataDiskSpec
	if s == "" {
		return spec, fmt.Errorf("empty spec")
	}
	for part := range strings.SplitSeq(s, ",") {
		rawKey, rawVal, ok := strings.Cut(part, "=")
		if !ok {
			return spec, fmt.Errorf("%q is not key=value", part)
		}
		key := strings.TrimSpace(rawKey)
		val := strings.TrimSpace(rawVal)
		switch key {
		case "size":
			n, err := ParseSize(val)
			if err != nil {
				return spec, fmt.Errorf("invalid size %q: %w", val, err)
			}
			if n < MinDataDiskSize {
				return spec, fmt.Errorf("size %s below the %s minimum", val, units.BytesSize(float64(MinDataDiskSize)))
			}
			spec.Size = n
		case "name":
			if !ValidDataDiskName(val) {
				return spec, fmt.Errorf("invalid name %q (must match [a-z][a-z0-9_-]{0,19}, no cocoon- prefix)", val)
			}
			spec.Name = val
		case "fstype":
			if !validDataDiskFSType(val) {
				return spec, fmt.Errorf("unsupported fstype %q (ext4 or none)", val)
			}
			spec.FSType = val
		case "mount":
			spec.MountPoint = val
			spec.MountPointSet = true
		case "directio":
			dio, err := ParseDirectIO(val)
			if err != nil {
				return spec, err
			}
			spec.DirectIO = dio
		default:
			return spec, fmt.Errorf("unknown key %q", key)
		}
	}
	if spec.Size == 0 {
		return spec, fmt.Errorf("size= required")
	}
	return spec, nil
}

// ParseDirectIO maps on/off/auto to a tri-state; auto is nil.
func ParseDirectIO(val string) (*bool, error) {
	switch val {
	case "on":
		t := true
		return &t, nil
	case "off":
		f := false
		return &f, nil
	case "auto":
		return nil, nil
	}
	return nil, fmt.Errorf("directio must be on/off/auto, got %q", val)
}

func validDataDiskFSType(t string) bool {
	return t == FSTypeExt4 || t == FSTypeNone
}
