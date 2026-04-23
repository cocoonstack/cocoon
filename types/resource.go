package types

// ResourceConfig holds the resource parameters shared between VMConfig
// and SnapshotConfig. Embedding it in both structs eliminates field
// duplication and allows value-copy transfer (e.g. BuildSnapshotConfig).
type ResourceConfig struct {
	CPU           int    `json:"cpu,omitempty"`
	Memory        int64  `json:"memory,omitempty"`          // bytes
	Storage       int64  `json:"storage,omitempty"`         // COW disk size, bytes
	QueueSize     int    `json:"queue_size,omitempty"`      // virtio-net ring depth per queue; 0 = default
	DiskQueueSize int    `json:"disk_queue_size,omitempty"` // virtio-blk ring depth per device; 0 = default
	Image         string `json:"image,omitempty"`
	ImageDigest   string `json:"image_digest,omitempty"` // resolved image digest (e.g. "sha256:abc123")
	ImageType     string `json:"image_type,omitempty"`   // image backend type ("oci" or "cloudimg")
	Network       string `json:"network,omitempty"`      // CNI conflist name; empty = default
	NoDirectIO    bool   `json:"no_direct_io,omitempty"` // disable O_DIRECT on writable disks
	Windows       bool   `json:"windows,omitempty"`      // Windows guest: UEFI boot, kvm_hyperv=on, no cidata
}
