package cloudhypervisor

// minBalloonMemory is the minimum guest memory (256 MiB) below which
// balloon is not enabled — the overhead is not worthwhile for tiny VMs.
const minBalloonMemory = 256 << 20

type chVMConfig struct {
	// Optional — pointer + omitempty (nil → omitted from JSON).
	Payload *chPayload     `json:"payload,omitempty"`
	Balloon *chBalloon     `json:"balloon,omitempty"`
	Serial  *chRuntimeFile `json:"serial,omitempty"`
	Console *chRuntimeFile `json:"console,omitempty"`

	// Required — value (always present).
	CPUs     chCPUs   `json:"cpus"`
	Memory   chMemory `json:"memory"`
	Disks    []chDisk `json:"disks,omitempty"`
	Nets     []chNet  `json:"net,omitempty"`
	RNG      chRNG    `json:"rng"`
	Watchdog bool     `json:"watchdog"`
}

type chNet struct {
	Tap       string `json:"tap"`
	Mac       string `json:"mac,omitempty"`
	NumQueues int64  `json:"num_queues,omitempty"`
	QueueSize int64  `json:"queue_size,omitempty"`

	// IP and Mask configure the host side of the tap interface.
	// nil → JSON null → CH skips tap IP configuration.
	// Without these fields CH defaults to 192.168.249.1/255.255.255.0
	// and tries to assign them, which fails in a managed netns (EINVAL).
	IP   *string `json:"ip"`
	Mask *string `json:"mask"`

	OffloadTSO  bool `json:"offload_tso,omitempty"`
	OffloadUFO  bool `json:"offload_ufo,omitempty"`
	OffloadCsum bool `json:"offload_csum,omitempty"`
}

type chPayload struct {
	Firmware  string `json:"firmware,omitempty"`
	Kernel    string `json:"kernel,omitempty"`
	Initramfs string `json:"initramfs,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
}

type chCPUs struct {
	BootVCPUs int `json:"boot_vcpus"`
	MaxVCPUs  int `json:"max_vcpus"`
}

type chMemory struct {
	Size      int64 `json:"size"`
	HugePages bool  `json:"hugepages,omitempty"`
}

type chDisk struct {
	Path         string `json:"path"`
	ReadOnly     bool   `json:"readonly,omitempty"`
	Direct       bool   `json:"direct,omitempty"`
	Sparse       bool   `json:"sparse,omitempty"`
	ImageType    string `json:"image_type,omitempty"`
	BackingFiles bool   `json:"backing_files,omitempty"`
	NumQueues    int    `json:"num_queues,omitempty"`
	QueueSize    int    `json:"queue_size,omitempty"`
	Serial       string `json:"serial,omitempty"`
}

type chBalloon struct {
	Size              int64 `json:"size"`
	DeflateOnOOM      bool  `json:"deflate_on_oom,omitempty"`
	FreePageReporting bool  `json:"free_page_reporting,omitempty"`
}

type chRNG struct {
	Src string `json:"src"`
}

type chRuntimeFile struct {
	Mode   string `json:"mode"`
	File   string `json:"file,omitempty"`
	Socket string `json:"socket,omitempty"`
}

type chVMInfoResponse struct {
	Config struct {
		Serial  chRuntimeFile `json:"serial"`
		Console chRuntimeFile `json:"console"`
	} `json:"config"`
}
