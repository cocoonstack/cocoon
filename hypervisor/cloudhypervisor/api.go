package cloudhypervisor

import "encoding/json"

type chVMConfig struct {
	// Optional — nil omitted from JSON.
	Payload *chPayload     `json:"payload,omitempty"`
	Balloon *chBalloon     `json:"balloon,omitempty"`
	Serial  *chRuntimeFile `json:"serial,omitempty"`
	Console *chRuntimeFile `json:"console,omitempty"`
	Vsock   *chVsock       `json:"vsock,omitempty"`

	// Required.
	CPUs     chCPUs   `json:"cpus"`
	Memory   chMemory `json:"memory"`
	Disks    []chDisk `json:"disks,omitempty"`
	Nets     []chNet  `json:"net,omitempty"`
	RNG      chRNG    `json:"rng"`
	Watchdog bool     `json:"watchdog"`
}

type chNet struct {
	ID        string `json:"id,omitempty"`
	TAP       string `json:"tap"`
	MAC       string `json:"mac,omitempty"`
	NumQueues int    `json:"num_queues,omitempty"`
	QueueSize int    `json:"queue_size,omitempty"`

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
	BootVCPUs int  `json:"boot_vcpus"`
	MaxVCPUs  int  `json:"max_vcpus"`
	KVMHyperV bool `json:"kvm_hyperv,omitempty"`
}

type chMemory struct {
	Size      int64 `json:"size"`
	HugePages bool  `json:"hugepages,omitempty"`
	Shared    bool  `json:"shared,omitempty"`
}

type chDisk struct {
	ID            string            `json:"id,omitempty"`
	Path          string            `json:"path"`
	ReadOnly      bool              `json:"readonly,omitempty"`
	DirectIO      bool              `json:"direct,omitempty"`
	Sparse        bool              `json:"sparse,omitempty"`
	ImageType     string            `json:"image_type,omitempty"`
	BackingFiles  bool              `json:"backing_files,omitempty"`
	NumQueues     int               `json:"num_queues,omitempty"`
	QueueSize     int               `json:"queue_size,omitempty"`
	QueueAffinity []chQueueAffinity `json:"queue_affinity,omitempty"`
	Serial        string            `json:"serial,omitempty"`
}

type chQueueAffinity struct {
	QueueIndex int   `json:"queue_index"`
	HostCPUs   []int `json:"host_cpus"`
}

type chBalloon struct {
	ID                string `json:"id,omitempty"`
	Size              int64  `json:"size"`
	DeflateOnOOM      bool   `json:"deflate_on_oom,omitempty"`
	FreePageReporting bool   `json:"free_page_reporting,omitempty"`
}

type chRNG struct {
	Src string `json:"src"`
}

type chRuntimeFile struct {
	Mode   string `json:"mode"`
	File   string `json:"file,omitempty"`
	Socket string `json:"socket,omitempty"`
}

type chVsock struct {
	CID    uint32 `json:"cid"`
	Socket string `json:"socket"`
}

type chFs struct {
	ID        string `json:"id,omitempty"`
	Tag       string `json:"tag"`
	Socket    string `json:"socket"`
	NumQueues int    `json:"num_queues,omitempty"`
	QueueSize int    `json:"queue_size,omitempty"`
}

type chDevice struct {
	ID   string `json:"id,omitempty"`
	Path string `json:"path"`
}

// chPciDeviceInfo is the response body from vm.add-fs / vm.add-device / vm.add-disk / vm.add-net (HTTP 200).
type chPciDeviceInfo struct {
	ID  string `json:"id"`
	BDF string `json:"bdf"`
}

type chVMInfoResponse struct {
	Config     chVMInfoConfig             `json:"config"`
	DeviceTree map[string]json.RawMessage `json:"device_tree,omitempty"`
}

type chVMInfoConfig struct {
	Serial  chRuntimeFile `json:"serial"`
	Console chRuntimeFile `json:"console"`
	Memory  chMemory      `json:"memory"`
	Disks   []chDisk      `json:"disks,omitempty"`
	Fs      []chFs        `json:"fs,omitempty"`
	Devices []chDevice    `json:"devices,omitempty"`
	Nets    []chNet       `json:"net,omitempty"`
}
