package types

// Image backend type names (Config.ImageType / Images.Type()).
const (
	ImageTypeOCI      = "oci"
	ImageTypeCloudImg = "cloudimg"
)

// Config holds resource params shared by VMConfig and SnapshotConfig (value-copy friendly).
type Config struct {
	CPU           int    `json:"cpu,omitzero"`
	Memory        int64  `json:"memory,omitzero"`          // bytes
	Storage       int64  `json:"storage,omitzero"`         // COW disk size, bytes
	QueueSize     int    `json:"queue_size,omitzero"`      // virtio-net ring depth per queue; 0 = default
	DiskQueueSize int    `json:"disk_queue_size,omitzero"` // virtio-blk ring depth per device; 0 = default
	Image         string `json:"image,omitempty"`
	ImageDigest   string `json:"image_digest,omitempty"` // resolved image digest (e.g. "sha256:abc123")
	ImageType     string `json:"image_type,omitempty"`
	Network       string `json:"network,omitempty"`     // CNI conflist name; empty = default
	NoDirectIO    bool   `json:"no_direct_io,omitzero"` // disable O_DIRECT on writable disks
	NoWatchdog    bool   `json:"no_watchdog,omitzero"`  // omit the virtio watchdog device (guest/driver compatibility)
	NoBalloon     bool   `json:"no_balloon,omitzero"`   // omit the virtio-balloon device (guests that thrash before deflate-on-OOM fires)
	Windows       bool   `json:"windows,omitzero"`      // Windows guest: UEFI boot, kvm_hyperv=on, no cidata
	// The four toggles below are fixed at create and persist through clone/restore.
	// SharedMemory toggles CH memory shared=on (vhost-user-fs prerequisite).
	SharedMemory bool `json:"shared_memory,omitzero"`
	// HugePages backs CH guest memory with hugetlbfs (costs snapshots the mmap fast path).
	HugePages bool `json:"hugepages,omitzero"`
	// Mergeable marks CH guest memory MADV_MERGEABLE for host KSM dedup (needs plain private memory).
	Mergeable bool `json:"mergeable,omitzero"`
	// PCI boots a Firecracker VM on the virtio-pci transport (device hot-plug prerequisite).
	PCI bool `json:"pci,omitzero"`

	// Raw cgroup v2 CPU knobs; zero derives the Guaranteed-at-N defaults from CPU (CPUSetCPUs empty = no placement).
	CPUWeight   int    `json:"cpu_weight,omitzero"`
	CPUQuotaUs  int64  `json:"cpu_quota_us,omitzero"`
	CPUPeriodUs int64  `json:"cpu_period_us,omitzero"`
	CPUBurstUs  int64  `json:"cpu_burst_us,omitzero"` // -1 = no burst; zero derives quota
	CPUSetCPUs  string `json:"cpuset_cpus,omitempty"`
}
