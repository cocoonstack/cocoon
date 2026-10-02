package cloudhypervisor

import "encoding/json/jsontext"

type chVMConfig struct {
	Payload *chPayload     `json:"payload,omitzero"`
	Balloon *chBalloon     `json:"balloon,omitzero"`
	Serial  *chRuntimeFile `json:"serial,omitzero"`
	Console *chRuntimeFile `json:"console,omitzero"`
	Vsock   *chVsock       `json:"vsock,omitzero"`

	CPUs     chCPUs     `json:"cpus"`
	Memory   chMemory   `json:"memory"`
	Disks    []chDisk   `json:"disks,omitempty"`
	Fs       []chFs     `json:"fs,omitempty"`
	Devices  []chDevice `json:"devices,omitempty"`
	Nets     []chNet    `json:"net,omitempty"`
	RNG      chRNG      `json:"rng"`
	Watchdog bool       `json:"watchdog"`
}

type chNet struct {
	ID        string `json:"id,omitempty"`
	TAP       string `json:"tap"`
	MAC       string `json:"mac,omitempty"`
	NumQueues int    `json:"num_queues,omitzero"`
	QueueSize int    `json:"queue_size,omitzero"`

	OffloadTSO  bool `json:"offload_tso,omitzero"`
	OffloadUFO  bool `json:"offload_ufo,omitzero"`
	OffloadCsum bool `json:"offload_csum,omitzero"`
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
	KVMHyperV bool `json:"kvm_hyperv,omitzero"`
}

type chMemory struct {
	Size      int64 `json:"size"`
	HugePages bool  `json:"hugepages,omitzero"`
	Shared    bool  `json:"shared,omitzero"`
	Mergeable bool  `json:"mergeable,omitzero"`
}

type chDisk struct {
	ID            string            `json:"id,omitempty"`
	Path          string            `json:"path"`
	ReadOnly      bool              `json:"readonly,omitzero"`
	DirectIO      bool              `json:"direct,omitzero"`
	Sparse        bool              `json:"sparse,omitzero"`
	ImageType     string            `json:"image_type,omitempty"`
	BackingFiles  bool              `json:"backing_files,omitzero"`
	NumQueues     int               `json:"num_queues,omitzero"`
	QueueSize     int               `json:"queue_size,omitzero"`
	QueueAffinity []chQueueAffinity `json:"queue_affinity,omitempty"`
	Serial        string            `json:"serial,omitempty"`
}

type chQueueAffinity struct {
	QueueIndex int   `json:"queue_index"`
	HostCPUs   []int `json:"host_cpus"`
}

type chBalloon struct {
	Size              int64 `json:"size"`
	DeflateOnOOM      bool  `json:"deflate_on_oom,omitzero"`
	FreePageReporting bool  `json:"free_page_reporting,omitzero"`
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
	NumQueues int    `json:"num_queues,omitzero"`
	QueueSize int    `json:"queue_size,omitzero"`
}

type chDevice struct {
	ID   string `json:"id,omitempty"`
	Path string `json:"path"`
}

// chPciDeviceInfo is the response body from vm.add-fs / vm.add-device / vm.add-disk / vm.add-net (HTTP 200).
type chPciDeviceInfo struct {
	ID string `json:"id"`
}

type chVMInfoResponse struct {
	State            string                    `json:"state,omitempty"`
	Config           chVMInfoConfig            `json:"config"`
	MemoryActualSize int64                     `json:"memory_actual_size,omitzero"`
	DeviceTree       map[string]jsontext.Value `json:"device_tree,omitempty"`
}

type chVMInfoConfig struct {
	Console chRuntimeFile `json:"console"`
	Memory  chMemory      `json:"memory"`
	Balloon *chBalloon    `json:"balloon,omitzero"`
	Disks   []chDisk      `json:"disks,omitempty"`
	Fs      []chFs        `json:"fs,omitempty"`
	Devices []chDevice    `json:"devices,omitempty"`
	Nets    []chNet       `json:"net,omitempty"`
}
