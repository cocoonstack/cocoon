package types

// Image backend type names (Config.ImageType / Images.Type()).
const (
	ImageTypeOCI      = "oci"
	ImageTypeCloudImg = "cloudimg"
)

// Config holds resource params shared by VMConfig and SnapshotConfig (value-copy friendly).
type Config struct {
	CPU           int    `json:"cpu,omitempty"`
	Memory        int64  `json:"memory,omitempty"`          // bytes
	Storage       int64  `json:"storage,omitempty"`         // COW disk size, bytes
	QueueSize     int    `json:"queue_size,omitempty"`      // virtio-net ring depth per queue; 0 = default
	DiskQueueSize int    `json:"disk_queue_size,omitempty"` // virtio-blk ring depth per device; 0 = default
	Image         string `json:"image,omitempty"`
	ImageDigest   string `json:"image_digest,omitempty"` // resolved image digest (e.g. "sha256:abc123")
	ImageType     string `json:"image_type,omitempty"`   // backend type, ImageTypeOCI / ImageTypeCloudImg
	Network       string `json:"network,omitempty"`      // CNI conflist name; empty = default
	NoDirectIO    bool   `json:"no_direct_io,omitempty"` // disable O_DIRECT on writable disks
	Windows       bool   `json:"windows,omitempty"`      // Windows guest: UEFI boot, kvm_hyperv=on, no cidata
	// SharedMemory toggles CH memory shared=on (vhost-user-fs prerequisite); fixed at create, persists through clone/restore.
	SharedMemory bool `json:"shared_memory,omitempty"`
	// HugePages backs CH guest memory with hugetlbfs; snapshots of such a VM lose the mmap restore fast path. Fixed at create, persists through clone/restore.
	HugePages bool `json:"hugepages,omitempty"`
}
